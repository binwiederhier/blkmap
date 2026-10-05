package device

import (
	"errors"
	"io"
	"log"
	"sync/atomic"
	"time"

	"heckel.io/blkmap/cow"
)

const (
	// maxLoggedErrors caps I/O and hydration error lines so a dead source cannot flood the
	// journal; the totals stay visible in the hydration report and the counters.
	maxLoggedErrors = 20
)

// backend wraps the store so I/O failures are visible in the log (the ublk layer itself
// only turns them into EIO for the kernel), so hydration can tell when the guest is busy,
// and to count requests for the status socket. Counting is atomic and allocation-free.
type backend struct {
	store      *cow.Store
	id         string
	logged     atomic.Int64
	inflight   atomic.Int64
	lastActive atomic.Int64 // unix nanoseconds of the last completed request
	reads      atomic.Int64
	writes     atomic.Int64
	flushes    atomic.Int64
	discards   atomic.Int64
	zeroes     atomic.Int64
	readBytes  atomic.Int64
	writeBytes atomic.Int64
	errors     atomic.Int64
	readLat    latency
	writeLat   latency
	rec        atomic.Pointer[ioRecorder] // nil unless a recording is running
}

// latency is a histogram over latencyBuckets, one bucket more for the overflow.
type latency struct {
	counts [len(latencyBucketNanos) + 1]atomic.Int64
	count  atomic.Int64
	nanos  atomic.Int64
}

// busy reports whether guest I/O is in flight or finished within hydrateBackoff.
func (b *backend) busy() bool {
	return b.inflight.Load() > 0 || time.Since(time.Unix(0, b.lastActive.Load())) < hydrateBackoff
}

func (b *backend) enter() {
	b.inflight.Add(1)
}

func (b *backend) leave() {
	b.lastActive.Store(time.Now().UnixNano())
	b.inflight.Add(-1)
}

func (b *backend) ReadAt(p []byte, off int64) (int, error) {
	b.enter()
	defer b.leave()
	start := time.Now()
	n, err := b.store.ReadAt(p, off)
	b.readLat.observe(time.Since(start))
	b.record(false, off, int64(len(p)), start)
	b.reads.Add(1)
	b.count(&b.readBytes, n, err)
	b.logError("read", off, len(p), err)
	return n, err
}

func (b *backend) WriteAt(p []byte, off int64) (int, error) {
	b.enter()
	defer b.leave()
	start := time.Now()
	n, err := b.store.WriteAt(p, off)
	b.writeLat.observe(time.Since(start))
	b.record(true, off, int64(len(p)), start)
	b.writes.Add(1)
	b.count(&b.writeBytes, n, err)
	b.logError("write", off, len(p), err)
	return n, err
}

// count adds the bytes of a successful transfer, or an error.
func (b *backend) count(bytes *atomic.Int64, n int, err error) {
	if err != nil && !errors.Is(err, io.EOF) {
		b.errors.Add(1)
		return
	}
	bytes.Add(int64(n))
}

func (b *backend) Size() int64 {
	return b.store.Size()
}

func (b *backend) Flush() error {
	b.enter()
	defer b.leave()
	err := b.store.Flush()
	b.flushes.Add(1)
	b.countErr(err)
	b.logError("flush", 0, 0, err)
	return err
}

func (b *backend) Discard(off, length int64) error {
	b.enter()
	defer b.leave()
	err := b.store.Discard(off, length)
	b.discards.Add(1)
	b.countErr(err)
	b.logError("discard", off, int(length), err)
	return err
}

func (b *backend) WriteZeroes(off, length int64) error {
	b.enter()
	defer b.leave()
	err := b.store.WriteZeroes(off, length)
	b.zeroes.Add(1)
	b.countErr(err)
	b.logError("write zeroes", off, int(length), err)
	return err
}

func (b *backend) countErr(err error) {
	if err != nil {
		b.errors.Add(1)
	}
}

// stats returns the request counters.
func (b *backend) stats() IOStats {
	return IOStats{
		Reads:        b.reads.Load(),
		Writes:       b.writes.Load(),
		Flushes:      b.flushes.Load(),
		Discards:     b.discards.Load(),
		WriteZeroes:  b.zeroes.Load(),
		ReadBytes:    b.readBytes.Load(),
		WriteBytes:   b.writeBytes.Load(),
		Errors:       b.errors.Load(),
		Inflight:     b.inflight.Load(),
		ReadLatency:  b.readLat.snapshot(),
		WriteLatency: b.writeLat.snapshot(),
	}
}

func (l *latency) observe(d time.Duration) {
	i := 0
	for i < len(latencyBucketNanos) && int64(d) > latencyBucketNanos[i] {
		i++
	}
	l.counts[i].Add(1)
	l.count.Add(1)
	l.nanos.Add(int64(d))
}

func (l *latency) snapshot() Histogram {
	h := Histogram{Counts: make([]int64, len(l.counts)), Count: l.count.Load(), Sum: time.Duration(l.nanos.Load()).Seconds()}
	for i := range l.counts {
		h.Counts[i] = l.counts[i].Load()
	}
	return h
}

func (b *backend) logError(op string, off int64, length int, err error) {
	if err == nil {
		return
	}
	if n := b.logged.Add(1); n > maxLoggedErrors {
		return
	} else if n == maxLoggedErrors {
		log.Printf("%s: %s at offset %d (%d bytes) failed: %s (further I/O errors not logged)", b.id, op, off, length, err.Error())
		return
	}
	log.Printf("%s: %s at offset %d (%d bytes) failed: %s", b.id, op, off, length, err.Error())
}
