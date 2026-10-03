package source

import (
	"errors"
	"io"
	"sync/atomic"
)

var (
	// ErrNotFound is returned (or wrapped) by a cache tier for a block it does not have. A
	// Cache treats it as a miss; any other error from the fast tier counts as a failure.
	// Both fall through to the slow source.
	ErrNotFound = errors.New("block not found")
)

// Cache reads from a fast source first and falls back to a slow one on any error, without
// retries. The fast tier is never written; something else manages it. A fast tier that can
// tell where its data is (Present: a sparse file, a mapped source) is asked first, so a
// partial copy misses on what it lacks instead of serving zeros for it.
type Cache struct {
	fast     Source
	slow     Source
	present  Present // fast, when it can tell what it holds; else nil
	hits     atomic.Int64
	misses   atomic.Int64
	failures atomic.Int64
}

// CacheStats counts how reads were served.
type CacheStats struct {
	Hits     int64 // served by the fast tier
	Misses   int64 // fast tier answered ErrNotFound
	Failures int64 // fast tier failed some other way
}

// NewCache returns a Cache. The device size is the slow source's size.
func NewCache(fast, slow Source) *Cache {
	c := &Cache{fast: fast, slow: slow}
	c.present, _ = fast.(Present)
	return c
}

func (c *Cache) ReadAt(p []byte, off int64) (int, error) {
	n, eof := clampRead(len(p), off, c.Size())
	switch read, err := c.readFast(p[:n], off); {
	case err == nil && read == n:
		c.hits.Add(1)
		return n, eof
	case err == nil || errors.Is(err, ErrNotFound):
		// A short read from the fast tier (it may be smaller than the slow source) is a miss too
		c.misses.Add(1)
	default:
		c.failures.Add(1)
	}
	read, err := c.slow.ReadAt(p[:n], off)
	if err != nil && !(errors.Is(err, io.EOF) && read == n) {
		return read, err
	}
	if read < n { // the unread rest of p may hold the fast tier's partial answer
		return read, io.ErrUnexpectedEOF
	}
	return n, eof
}

// Abort aborts both tiers.
func (c *Cache) Abort() {
	Abort(c.fast)
	Abort(c.slow)
}

// readFast reads from the fast tier, or reports ErrNotFound without reading when the tier
// knows it does not hold the whole range.
func (c *Cache) readFast(p []byte, off int64) (int, error) {
	if c.present != nil && !c.present.Present(off, int64(len(p))) {
		return 0, ErrNotFound
	}
	return c.fast.ReadAt(p, off)
}

// ReadAtDirect reads from the slow source only.
func (c *Cache) ReadAtDirect(p []byte, off int64) (int, error) {
	return ReadDirect(c.slow, p, off)
}

// Holes come from the slow source, the authority on content.
func (c *Cache) Holes(off, length int64) ([]Range, error) {
	return Holes(c.slow, off, length)
}

func (c *Cache) Size() int64 {
	return c.slow.Size()
}

func (c *Cache) Stats() CacheStats {
	return CacheStats{Hits: c.hits.Load(), Misses: c.misses.Load(), Failures: c.failures.Load()}
}

func (c *Cache) Close() error {
	return errors.Join(c.fast.Close(), c.slow.Close())
}
