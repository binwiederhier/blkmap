// mirrorcrash records a mirror group's crash states and checks them, for crashreplay.sh
// MODE=mirror: two devices served as a group the way a mirror runs (m1's base is a live view
// of m0, nopwrite and reclaim on both), and a writer whose writes reach the plexes in either
// order and get flushed at different times. A device's writes count as acknowledged when its
// flush returns; every acknowledged write must survive any crash, whatever the sibling did.
//
//	mirrorcrash record -dir D -base B -run R -records N [-mark DM] -journal J
//	mirrorcrash verify -dir D -base B -journal J -upto K
//
// The journal lists "W dev slot seq" before each write and "A K dev slot seq" for the writes
// a flush acknowledged (K counts the flushes that acknowledged something; with -mark each is
// followed by a dm-log-writes mark "ackK"). verify checks the stores in D, as a crash left
// them, against everything acknowledged up to K.
package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"

	"heckel.io/blkmap/cow"
	"heckel.io/blkmap/device"
	"heckel.io/blkmap/source"
)

const (
	recordSize = 4096
	chunkSize  = 4096 // one record per chunk, so chunks of both plexes are often identical
	size       = 16 << 20
	slots      = size / recordSize
	magic      = "MIRR"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mirrorcrash record|verify ...")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	dir := fs.String("dir", "", "directory of the COW files")
	base := fs.String("base", "", "m0's base image")
	run := fs.String("run", "", "run directory (live bitmaps); off the logged filesystem")
	records := fs.Int("records", 2000, "record: writes to issue")
	mark := fs.String("mark", "", "record: dm-log-writes device to mark acknowledgements in")
	journal := fs.String("journal", "", "the journal of writes and acknowledgements")
	upto := fs.Int("upto", -1, "verify: check acknowledgements up to this flush")
	fs.Parse(os.Args[2:])
	var err error
	switch os.Args[1] {
	case "record":
		err = record(*dir, *base, *run, *journal, *mark, *records)
	case "verify":
		err = verify(*dir, *base, *journal, *upto)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mirrorcrash:", err)
		os.Exit(1)
	}
}

// plexView is m1's base: m0 as the guest sees it, through m0's store, and what of it is
// durable, so m1 relies only on that. Durable is asserted against a local interface so the
// program also builds against releases without source.Durability.
type plexView struct {
	lookup source.Lookup
}

func (v *plexView) Bind(lookup source.Lookup) { v.lookup = lookup }
func (v *plexView) Size() int64               { return size }
func (v *plexView) Close() error              { return nil }
func (v *plexView) ReadAt(p []byte, off int64) (int, error) {
	if v.lookup == nil {
		return 0, errors.New("not bound")
	}
	r, ok := v.lookup("m0")
	if !ok {
		return 0, errors.New("m0 not served")
	}
	return r.ReadAt(p, off)
}
func (v *plexView) Durable(off, length int64) bool {
	if v.lookup == nil {
		return false
	}
	r, ok := v.lookup("m0")
	if !ok {
		return false
	}
	d, ok := r.(interface{ Durable(int64, int64) bool })
	return ok && d.Durable(off, length)
}

func record(dir, base, run, journalPath, mark string, n int) error {
	b0, err := source.OpenFile(base, 0, 0)
	if err != nil {
		return err
	}
	opt := func(id string, src source.Source) *device.Options {
		return &device.Options{ID: id, Base: src, COWFile: filepath.Join(dir, id+".cow"), ChunkSize: chunkSize,
			DevDir: filepath.Join(run, "dev"), RunDir: run, NopWrite: true, Reclaim: true}
	}
	g, err := device.ServeGroup(context.Background(), []*device.Options{opt("m0", b0), opt("m1", &plexView{})})
	if err != nil {
		return err
	}
	defer g.Close()
	j, err := os.Create(journalPath)
	if err != nil {
		return err
	}
	defer j.Close()
	jw := bufio.NewWriter(j)
	defer jw.Flush()
	var fds [2]int
	for i, id := range []string{"m0", "m1"} {
		if fds[i], err = unix.Open(g.Devices[id].BlockPath, unix.O_RDWR|unix.O_DIRECT, 0); err != nil {
			return err
		}
		defer unix.Close(fds[i])
	}
	r := rand.New(rand.NewPCG(uint64(os.Getpid()), 7))
	buf := aligned()
	type pending struct{ slot, seq int }
	var unflushed [2][]pending
	acks := 0
	write := func(dev, slot, seq int) error {
		fmt.Fprintf(jw, "W %d %d %d\n", dev, slot, seq)
		copy(buf, encode(seq, slot))
		if _, err := unix.Pwrite(fds[dev], buf, int64(slot*recordSize)); err != nil {
			return fmt.Errorf("write m%d slot %d: %w", dev, slot, err)
		}
		unflushed[dev] = append(unflushed[dev], pending{slot, seq})
		return nil
	}
	flush := func(dev int) error {
		if err := unix.Fdatasync(fds[dev]); err != nil {
			return fmt.Errorf("flush m%d: %w", dev, err)
		}
		if len(unflushed[dev]) == 0 {
			return nil
		}
		acks++
		for _, p := range unflushed[dev] {
			fmt.Fprintf(jw, "A %d %d %d %d\n", acks, dev, p.slot, p.seq)
		}
		unflushed[dev] = unflushed[dev][:0]
		if err := jw.Flush(); err != nil {
			return err
		}
		if mark != "" {
			if out, err := exec.Command("dmsetup", "message", mark, "0", "mark", fmt.Sprintf("ack%d", acks)).CombinedOutput(); err != nil {
				return fmt.Errorf("mark: %w: %s", err, out)
			}
		}
		return nil
	}
	// A copy of a write one plex got alone, to give the other plex later: what a resync does,
	// and the order in which a sibling's unflushed copy tempts nopwrite and reclaim
	var later []pending
	for seq := 0; seq < n; seq++ {
		slot := r.IntN(slots)
		switch k := r.IntN(10); {
		case k < 4: // a mirrored write, halves and flushes in either order
			first := r.IntN(2)
			if err := errors.Join(write(first, slot, seq), write(1-first, slot, seq)); err != nil {
				return err
			}
			first = r.IntN(2)
			if err := errors.Join(flush(first), flush(1-first)); err != nil {
				return err
			}
		case k < 6: // m1 alone, flushed; m0 gets the same bytes later
			if err := errors.Join(write(1, slot, seq), flush(1)); err != nil {
				return err
			}
			later = append(later, pending{slot, seq})
		case k < 8: // m0 first, unflushed; then m1, flushed
			if err := errors.Join(write(0, slot, seq), write(1, slot, seq), flush(1)); err != nil {
				return err
			}
		default: // m0 alone, flushed
			if err := errors.Join(write(0, slot, seq), flush(0)); err != nil {
				return err
			}
		}
		for len(later) > 0 && r.IntN(2) == 0 { // the deferred copies, unflushed
			p := later[0]
			later = later[1:]
			if err := write(0, p.slot, p.seq); err != nil {
				return err
			}
		}
		if r.IntN(5) == 0 { // a plex flushes on its own schedule
			if err := flush(r.IntN(2)); err != nil {
				return err
			}
		}
	}
	if err := errors.Join(flush(0), flush(1)); err != nil {
		return err
	}
	// What the run exercised: chunks stored against writes issued, and the sweeper's work
	for _, id := range []string{"m0", "m1"} {
		st := g.Devices[id].Status()
		line := fmt.Sprintf("%s: %d writes, %d chunks stored", id, st.IO.Writes, st.Written)
		if st.Reclaim != nil {
			line += fmt.Sprintf(", reclaim examined %d, dropped %d", st.Reclaim.Examined, st.Reclaim.Chunks)
		}
		fmt.Println(line)
	}
	return nil
}

func verify(dir, base, journalPath string, upto int) error {
	data, err := os.ReadFile(journalPath)
	if err != nil {
		return err
	}
	want := [2]map[int]int{{}, {}}
	touched := map[int]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) == 4 && f[0] == "W":
			slot, _ := strconv.Atoi(f[2])
			touched[slot] = true
		case len(f) == 5 && f[0] == "A":
			k, _ := strconv.Atoi(f[1])
			if k > upto {
				continue
			}
			dev, _ := strconv.Atoi(f[2])
			slot, _ := strconv.Atoi(f[3])
			seq, _ := strconv.Atoi(f[4])
			if old, ok := want[dev][slot]; !ok || seq > old {
				want[dev][slot] = seq
			}
		}
	}
	b0, err := source.OpenFile(base, 0, 0)
	if err != nil {
		return err
	}
	baseData, err := os.ReadFile(base)
	if err != nil {
		return err
	}
	m0, err := cow.Open(b0, filepath.Join(dir, "m0.cow"), filepath.Join(dir, "m0.cow.bitmap"), chunkSize)
	if err != nil {
		return fmt.Errorf("open m0: %w", err)
	}
	defer m0.Close()
	m1, err := cow.Open(&storeReader{m0}, filepath.Join(dir, "m1.cow"), filepath.Join(dir, "m1.cow.bitmap"), chunkSize)
	if err != nil {
		return fmt.Errorf("open m1: %w", err)
	}
	defer m1.Close()
	var problems []string
	got := make([]byte, recordSize)
	for dev, s := range []*cow.Store{m0, m1} {
		for slot := 0; slot < slots; slot++ {
			if _, err := s.ReadAt(got, int64(slot*recordSize)); err != nil && !errors.Is(err, io.EOF) {
				return fmt.Errorf("read m%d slot %d: %w", dev, slot, err)
			}
			seq, recSlot, ok := decode(got)
			w, acked := want[dev][slot]
			switch {
			case ok && recSlot != slot:
				problems = append(problems, fmt.Sprintf("m%d slot %d holds slot %d's record", dev, slot, recSlot))
			case acked && (!ok || seq < w):
				problems = append(problems, fmt.Sprintf("m%d slot %d: acknowledged seq %d lost (valid %v, seq %d)", dev, slot, w, ok, seq))
			case !touched[slot] && string(got) != string(baseData[slot*recordSize:(slot+1)*recordSize]):
				problems = append(problems, fmt.Sprintf("m%d slot %d: never written, no longer reads as the base", dev, slot))
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%d problems:\n  %s", len(problems), strings.Join(problems[:min(10, len(problems))], "\n  "))
	}
	fmt.Printf("verified up to flush %d: %d and %d acknowledged slots\n", upto, len(want[0]), len(want[1]))
	return nil
}

// storeReader is m1's base at verify time: m0's store as a crash left it.
type storeReader struct{ s *cow.Store }

func (r *storeReader) ReadAt(p []byte, off int64) (int, error) { return r.s.ReadAt(p, off) }
func (r *storeReader) Size() int64                             { return r.s.Size() }
func (r *storeReader) Close() error                            { return nil }

func encode(seq, slot int) []byte {
	b := make([]byte, recordSize)
	copy(b, magic)
	binary.LittleEndian.PutUint64(b[4:], uint64(seq))
	binary.LittleEndian.PutUint64(b[12:], uint64(slot))
	for i := 24; i < recordSize; i++ {
		b[i] = byte(seq + i*31)
	}
	binary.LittleEndian.PutUint32(b[20:], crc(b))
	return b
}

func decode(b []byte) (seq, slot int, ok bool) {
	if string(b[:4]) != magic || binary.LittleEndian.Uint32(b[20:]) != crc(b) {
		return 0, 0, false
	}
	return int(binary.LittleEndian.Uint64(b[4:])), int(binary.LittleEndian.Uint64(b[12:])), true
}

func crc(b []byte) uint32 {
	return crc32.Update(crc32.ChecksumIEEE(b[:20]), crc32.IEEETable, b[24:recordSize])
}

func aligned() []byte {
	b := make([]byte, 2*recordSize)
	off := recordSize - int(uintptr(unsafe.Pointer(&b[0]))%recordSize)
	return b[off : off+recordSize]
}
