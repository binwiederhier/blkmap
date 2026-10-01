package source

import (
	"errors"
	"sync/atomic"
)

var (
	// ErrNotFound is returned (or wrapped) by a cache tier for a block it does not have. A
	// Cache treats it as a miss; any other error from the fast tier counts as a failure.
	// Both fall through to the slow source.
	ErrNotFound = errors.New("block not found")
)

// Cache reads from a fast source first and falls back to a slow one on any error, without
// retries. The fast tier is never written; something else manages it.
type Cache struct {
	fast     Source
	slow     Source
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
	return &Cache{fast: fast, slow: slow}
}

func (c *Cache) ReadAt(p []byte, off int64) (int, error) {
	n, eof := clampRead(len(p), off, c.Size())
	// A short read from the fast tier (it may be smaller than the slow source) is a miss too
	if read, err := c.fast.ReadAt(p[:n], off); err == nil && read == n {
		c.hits.Add(1)
		return n, eof
	} else if err == nil || errors.Is(err, ErrNotFound) {
		c.misses.Add(1)
	} else {
		c.failures.Add(1)
	}
	return c.slow.ReadAt(p, off)
}

// ReadAtDirect reads from the slow source only.
func (c *Cache) ReadAtDirect(p []byte, off int64) (int, error) {
	return ReadDirect(c.slow, p, off)
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
