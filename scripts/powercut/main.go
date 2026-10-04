// powercut writes and verifies checksummed records on a block device, for power-loss and
// crash tests; scripts/powercut.sh drives it.
//
//	powercut write DEV [-mark DM] [-count N]
//	                             write records (forever, or N), flushing every batch; prints
//	                             "start N" once and "ack N" after each flush, and with -mark
//	                             logs "ackN" in dm-log-writes device DM before going on
//	powercut verify DEV [-base FILE] A-B...
//	                             check that every acknowledged record (the A-B spans of
//	                             the writer runs) survived, and the rest still reads as FILE
//
// Record seq lives in slot slotOf(seq), so later records overwrite earlier ones. After a
// crash each slot must hold at least the newest acknowledged record mapped to it; newer,
// unacknowledged ones may or may not be there, and only an in-flight overwrite may leave a
// slot torn.
package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	recordSize = 4096
	magic      = "PCUT"
	// batch is how many records go between flushes; at most a batch can be in flight
	batch    = 16
	maxSlots = 16384
	// Record layout: magic[4] seq[8] slot[8] crc[4] payload
	offSeq     = 4
	offSlot    = 12
	offCRC     = 20
	offPayload = 24
	maxReports = 10
)

// span is an inclusive range of sequence numbers one writer run acknowledged.
type span struct {
	first, last uint64
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: powercut write DEV | powercut verify DEV A-B...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "write":
		fs := flag.NewFlagSet("write", flag.ExitOnError)
		mark := fs.String("mark", "", "dm-log-writes device to log an ack mark in after each flush")
		count := fs.Uint64("count", 0, "stop after this many records (0: never)")
		fs.Parse(os.Args[3:])
		err = write(os.Args[2], *mark, *count)
	case "verify":
		fs := flag.NewFlagSet("verify", flag.ExitOnError)
		base := fs.String("base", "", "file the device overlays: slots without a record must read as it")
		fs.Parse(os.Args[3:])
		var spans []span
		if spans, err = parseSpans(fs.Args()); err == nil {
			err = verify(os.Args[2], *base, spans)
		}
	default:
		err = fmt.Errorf("unknown mode %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "powercut:", err)
		os.Exit(1)
	}
	fmt.Println("OK")
}

// write continues after the newest record on the device and writes until killed, or count
// records.
func write(path, mark string, count uint64) error {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_DIRECT, 0)
	if err != nil {
		return err
	}
	size, err := unix.Seek(fd, 0, io.SeekEnd)
	if err != nil {
		return err
	}
	slots := min(uint64(size)/recordSize, maxSlots)
	buf := aligned()
	var start uint64
	for slot := uint64(0); slot < slots; slot++ {
		if _, err := unix.Pread(fd, buf, int64(slot*recordSize)); err != nil {
			return err
		}
		if seq, _, ok := decode(buf); ok && seq >= start {
			start = seq + 1
		}
	}
	fmt.Printf("start %d\n", start)
	for seq := start; count == 0 || seq < start+count; seq++ {
		slot := slotOf(seq, slots)
		copy(buf, encode(seq, slot))
		if _, err := unix.Pwrite(fd, buf, int64(slot*recordSize)); err != nil {
			return fmt.Errorf("write seq %d: %w", seq, err)
		}
		if (seq-start+1)%batch == 0 {
			if err := unix.Fdatasync(fd); err != nil {
				return fmt.Errorf("flush after seq %d: %w", seq, err)
			}
			fmt.Printf("ack %d\n", seq)
			if mark != "" {
				if out, err := exec.Command("dmsetup", "message", mark, "0", "mark", fmt.Sprintf("ack%d", seq)).CombinedOutput(); err != nil {
					return fmt.Errorf("mark ack %d: %w: %s", seq, err, out)
				}
			}
		}
	}
	return nil
}

// verify checks the device against the acknowledged spans and, given the base the device
// overlays, that every slot no record reached still reads as the base.
func verify(path, base string, acked []span) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	slots := min(uint64(size)/recordSize, maxSlots)
	expect := make(map[uint64]uint64) // slot -> newest acknowledged seq
	tornOK := make(map[uint64]bool)   // slot -> an unacknowledged overwrite was in flight
	for _, s := range acked {
		for seq := s.first; seq <= s.last; seq++ {
			slot := slotOf(seq, slots)
			if seq >= expect[slot] {
				expect[slot] = seq
			}
		}
		for seq := s.last + 1; seq <= s.last+batch; seq++ {
			tornOK[slotOf(seq, slots)] = true
		}
	}
	var baseFile *os.File
	if base != "" {
		if baseFile, err = os.Open(base); err != nil {
			return err
		}
		defer baseFile.Close()
	}
	var failures []string
	buf, baseBuf := make([]byte, recordSize), make([]byte, recordSize)
	for slot := uint64(0); slot < slots; slot++ {
		if _, err := f.ReadAt(buf, int64(slot*recordSize)); err != nil {
			return err
		}
		want, has := expect[slot]
		seq, recSlot, ok := decode(buf)
		baseLost := false
		if baseFile != nil && !ok && !has && !tornOK[slot] {
			if _, err := baseFile.ReadAt(baseBuf, int64(slot*recordSize)); err != nil {
				return err
			}
			baseLost = !bytes.Equal(buf, baseBuf)
		}
		switch {
		case baseLost:
			failures = append(failures, fmt.Sprintf("slot %d holds no record and no longer reads as the base", slot))
		case ok && recSlot != slot:
			failures = append(failures, fmt.Sprintf("slot %d holds seq %d of slot %d", slot, seq, recSlot))
		case ok && has && seq < want:
			failures = append(failures, fmt.Sprintf("slot %d: acknowledged seq %d lost, found older seq %d", slot, want, seq))
		case !ok && has && !tornOK[slot]:
			failures = append(failures, fmt.Sprintf("slot %d: acknowledged seq %d lost, slot is empty or torn", slot, want))
		}
	}
	if len(failures) > 0 {
		shown := failures[:min(len(failures), maxReports)]
		return fmt.Errorf("%d of %d slots wrong:\n  %s", len(failures), slots, strings.Join(shown, "\n  "))
	}
	fmt.Printf("verified %d slots, %d hold acknowledged records\n", slots, len(expect))
	return nil
}

// encode builds the record for seq in slot; the payload is derived from seq.
func encode(seq, slot uint64) []byte {
	b := make([]byte, recordSize)
	copy(b, magic)
	binary.LittleEndian.PutUint64(b[offSeq:], seq)
	binary.LittleEndian.PutUint64(b[offSlot:], slot)
	for i := offPayload; i < recordSize; i++ {
		b[i] = byte(seq + uint64(i)*31)
	}
	binary.LittleEndian.PutUint32(b[offCRC:], checksum(b))
	return b
}

// decode returns the record in b; ok is false for an empty, foreign or torn block.
func decode(b []byte) (seq, slot uint64, ok bool) {
	if len(b) < recordSize || string(b[:len(magic)]) != magic || binary.LittleEndian.Uint32(b[offCRC:]) != checksum(b) {
		return 0, 0, false
	}
	return binary.LittleEndian.Uint64(b[offSeq:]), binary.LittleEndian.Uint64(b[offSlot:]), true
}

func checksum(b []byte) uint32 {
	c := crc32.ChecksumIEEE(b[:offCRC])
	return crc32.Update(c, crc32.IEEETable, b[offPayload:recordSize])
}

// slotOf scatters sequence numbers over the slots (Fibonacci hashing).
func slotOf(seq, slots uint64) uint64 {
	return (seq * 0x9E3779B97F4A7C15 >> 17) % slots
}

func parseSpans(args []string) ([]span, error) {
	var spans []span
	for _, a := range args {
		first, last, ok := strings.Cut(a, "-")
		f, err1 := strconv.ParseUint(first, 10, 64)
		l, err2 := strconv.ParseUint(last, 10, 64)
		if !ok || err1 != nil || err2 != nil || l < f {
			return nil, fmt.Errorf("bad span %q, want FIRST-LAST", a)
		}
		spans = append(spans, span{f, l})
	}
	if len(spans) == 0 {
		return nil, errors.New("no acknowledged spans given")
	}
	return spans, nil
}

// aligned returns a page-aligned record buffer, as O_DIRECT requires.
func aligned() []byte {
	b := make([]byte, 2*recordSize)
	off := recordSize - int(uintptr(unsafe.Pointer(&b[0]))%recordSize)
	return b[off : off+recordSize]
}
