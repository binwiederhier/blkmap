package device

import (
	"log"
	"sync/atomic"
	"time"

	"heckel.io/blkmap/cow"
)

const (
	// maxLoggedErrors caps I/O error lines so a dead source cannot flood the journal.
	maxLoggedErrors = 20
)

// backend wraps the store so I/O failures are visible in the log (the ublk layer itself
// only turns them into EIO for the kernel) and so hydration can tell when the guest is busy.
type backend struct {
	store      *cow.Store
	id         string
	logged     atomic.Int64
	inflight   atomic.Int64
	lastActive atomic.Int64 // unix nanoseconds of the last completed request
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
	n, err := b.store.ReadAt(p, off)
	b.logError("read", off, len(p), err)
	return n, err
}

func (b *backend) WriteAt(p []byte, off int64) (int, error) {
	b.enter()
	defer b.leave()
	n, err := b.store.WriteAt(p, off)
	b.logError("write", off, len(p), err)
	return n, err
}

func (b *backend) Size() int64 {
	return b.store.Size()
}

func (b *backend) Flush() error {
	b.enter()
	defer b.leave()
	err := b.store.Flush()
	b.logError("flush", 0, 0, err)
	return err
}

func (b *backend) Discard(off, length int64) error {
	b.enter()
	defer b.leave()
	err := b.store.Discard(off, length)
	b.logError("discard", off, int(length), err)
	return err
}

func (b *backend) WriteZeroes(off, length int64) error {
	b.enter()
	defer b.leave()
	err := b.store.WriteZeroes(off, length)
	b.logError("write zeroes", off, int(length), err)
	return err
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
