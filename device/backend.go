package device

import (
	"log"
	"sync/atomic"

	"heckel.io/blkmap/cow"
)

const (
	// maxLoggedErrors caps I/O error lines so a dead source cannot flood the journal.
	maxLoggedErrors = 20
)

// backend wraps the store so I/O failures are visible in the log; the ublk layer itself
// only turns them into EIO for the kernel.
type backend struct {
	store  *cow.Store
	id     string
	logged atomic.Int64
}

func (b *backend) ReadAt(p []byte, off int64) (int, error) {
	n, err := b.store.ReadAt(p, off)
	b.logError("read", off, len(p), err)
	return n, err
}

func (b *backend) WriteAt(p []byte, off int64) (int, error) {
	n, err := b.store.WriteAt(p, off)
	b.logError("write", off, len(p), err)
	return n, err
}

func (b *backend) Size() int64 {
	return b.store.Size()
}

func (b *backend) Flush() error {
	err := b.store.Flush()
	b.logError("flush", 0, 0, err)
	return err
}

func (b *backend) Discard(off, length int64) error {
	err := b.store.Discard(off, length)
	b.logError("discard", off, int(length), err)
	return err
}

func (b *backend) WriteZeroes(off, length int64) error {
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
