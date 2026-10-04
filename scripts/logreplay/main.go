// logreplay reads a dm-log-writes log and rebuilds crash states from it, for crash-point
// testing; scripts/crashreplay.sh drives it.
//
//	logreplay plan LOG [STEP]                    one line per checked flush: Q P ACK
//	logreplay apply LOG TARGET FROM TO [KEEP SEED] apply entries FROM..TO-1 onto TARGET
//
// A crash just before flush entry Q completes leaves everything up to the previous flush P,
// every FUA write logged since (durable once complete), and any subset of the plain writes
// logged since: dm-log-writes logs plain writes only when a flush gathers them. ACK is the
// newest "ackN" mark logged before Q (-1 for none): the writer had seen record N flushed.
package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	logMagic    = 0x6a736677736872 // WRITE_LOG_MAGIC in drivers/md/dm-log-writes.c
	logVersion  = 1
	flagFlush   = 1 << 0
	flagFUA     = 1 << 1
	flagDiscard = 1 << 2
	flagMark    = 1 << 3
	entrySize   = 32 // struct log_write_entry
	markPrefix  = "ack"
)

// entry is one logged request; sector and nrSectors count the log's sector size.
type entry struct {
	sector, nrSectors, flags uint64
	mark                     string
	dataOff                  int64 // where the data starts in the log
}

// checkpoint is a crash state to check: just before flush entry q, after flush entry p.
type checkpoint struct{ q, p, ack int }

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "logreplay:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: logreplay plan LOG [STEP] | logreplay apply LOG TARGET FROM TO [KEEP SEED]")
	}
	log, err := os.Open(args[1])
	if err != nil {
		return err
	}
	defer log.Close()
	ss, entries, err := readLog(log)
	if err != nil {
		return err
	}
	switch {
	case args[0] == "plan":
		step := 1
		if len(args) > 2 {
			if step, err = strconv.Atoi(args[2]); err != nil || step < 1 {
				return fmt.Errorf("bad step %q", args[2])
			}
		}
		for _, c := range plan(entries, step) {
			fmt.Printf("%d %d %d\n", c.q, c.p, c.ack)
		}
		return nil
	case args[0] == "apply" && (len(args) == 5 || len(args) == 7):
		from, err1 := strconv.Atoi(args[3])
		to, err2 := strconv.Atoi(args[4])
		if err := errors.Join(err1, err2); err != nil || from < 0 || to < from || to > len(entries) {
			return fmt.Errorf("bad range %s..%s of %d entries", args[3], args[4], len(entries))
		}
		keep, r := 1.0, (*rand.Rand)(nil)
		if len(args) == 7 {
			seed, err2 := strconv.ParseUint(args[6], 10, 64)
			if keep, err1 = strconv.ParseFloat(args[5], 64); errors.Join(err1, err2) != nil {
				return fmt.Errorf("bad keep %q or seed %q", args[5], args[6])
			}
			r = rand.New(rand.NewPCG(seed, 0))
		}
		target, err := os.OpenFile(args[2], os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer target.Close()
		if err := apply(log, target, ss, entries, from, to, keep, r); err != nil {
			return err
		}
		return target.Sync()
	}
	return fmt.Errorf("unknown command %q", strings.Join(args, " "))
}

// readLog parses the super block and every entry header.
func readLog(f *os.File) (int64, []entry, error) {
	super := make([]byte, 28)
	if _, err := f.ReadAt(super, 0); err != nil {
		return 0, nil, fmt.Errorf("read super block: %w", err)
	}
	if binary.LittleEndian.Uint64(super[0:]) != logMagic || binary.LittleEndian.Uint64(super[8:]) != logVersion {
		return 0, nil, errors.New("not a dm-log-writes log (bad magic or version)")
	}
	count := binary.LittleEndian.Uint64(super[16:])
	ss := int64(binary.LittleEndian.Uint32(super[24:]))
	if ss < 512 || ss&(ss-1) != 0 {
		return 0, nil, fmt.Errorf("bad sector size %d", ss)
	}
	entries := make([]entry, 0, count)
	hdr := make([]byte, ss)
	off := ss
	for i := uint64(0); i < count; i++ {
		if _, err := f.ReadAt(hdr, off); err != nil {
			return 0, nil, fmt.Errorf("entry %d at %d: %w", i, off, err)
		}
		e := entry{
			sector:    binary.LittleEndian.Uint64(hdr[0:]),
			nrSectors: binary.LittleEndian.Uint64(hdr[8:]),
			flags:     binary.LittleEndian.Uint64(hdr[16:]),
		}
		dataLen := binary.LittleEndian.Uint64(hdr[24:])
		if e.flags&flagMark != 0 {
			e.mark = strings.TrimRight(string(hdr[entrySize:entrySize+min(dataLen, uint64(ss-entrySize))]), "\x00")
		} else if dataLen != 0 {
			return 0, nil, fmt.Errorf("entry %d: inline data is not supported", i)
		}
		off += ss
		e.dataOff = off
		if e.flags&flagDiscard == 0 {
			off += int64(e.nrSectors) * ss
		}
		entries = append(entries, e)
	}
	return ss, entries, nil
}

// apply writes entries [from, to) to target. With keep < 1 each plain write (neither FUA nor
// flush) is applied with probability keep; discards are applied as holes.
func apply(log, target *os.File, ss int64, entries []entry, from, to int, keep float64, r *rand.Rand) error {
	for i, e := range entries[from:to] {
		if e.flags&flagMark != 0 || e.nrSectors == 0 {
			continue
		}
		if plain := e.flags&(flagFUA|flagFlush) == 0; plain && keep < 1 && r.Float64() >= keep {
			continue
		}
		off, n := int64(e.sector)*ss, int64(e.nrSectors)*ss
		if e.flags&flagDiscard != 0 {
			if err := unix.Fallocate(int(target.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, off, n); err != nil {
				return fmt.Errorf("entry %d: discard: %w", from+i, err)
			}
			continue
		}
		if _, err := io.Copy(io.NewOffsetWriter(target, off), io.NewSectionReader(log, e.dataOff, n)); err != nil {
			return fmt.Errorf("entry %d: %w", from+i, err)
		}
	}
	return nil
}

// plan lists every step-th flush entry with the flush before it and the newest ack mark.
func plan(entries []entry, step int) []checkpoint {
	var points []checkpoint
	p, ack, flushes := -1, -1, 0
	for q, e := range entries {
		if n, ok := strings.CutPrefix(e.mark, markPrefix); ok {
			if v, err := strconv.Atoi(n); err == nil {
				ack = v
			}
		}
		if e.flags&flagFlush == 0 {
			continue
		}
		if flushes%step == 0 {
			points = append(points, checkpoint{q: q, p: p, ack: ack})
		}
		flushes++
		p = q
	}
	return points
}
