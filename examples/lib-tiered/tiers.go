package main

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"heckel.io/blkmap/source"
)

const (
	tierBlock = 1 << 20
)

var (
	errAborted = errors.New("aborted")
)

// memTier is a fast tier of your own: a block store (here a map in memory; in real life a
// key-value store, an object cache, a local NVMe directory) that holds some 1 MiB blocks.
// It answers source.ErrNotFound for blocks it does not hold, and implements source.Present
// so the cache asks it before reading: a range it does not fully hold goes to the slow tier
// without a failed read.
type memTier struct {
	size   int64
	blocks map[int64][]byte
	mu     sync.RWMutex // Protects blocks
}

var (
	_ source.Source  = (*memTier)(nil)
	_ source.Present = (*memTier)(nil)
)

func newMemTier(size int64) *memTier {
	return &memTier{size: size, blocks: make(map[int64][]byte)}
}

// fill copies blocks [from, to) of src into the tier, the way whatever manages the cache
// would populate it; blkmap never writes to a fast tier.
func (m *memTier) fill(src source.Source, from, to int64) error {
	for b := from; b < to && b*tierBlock < m.size; b++ {
		data := make([]byte, min(tierBlock, m.size-b*tierBlock))
		if _, err := src.ReadAt(data, b*tierBlock); err != nil {
			return err
		}
		m.mu.Lock()
		m.blocks[b] = data
		m.mu.Unlock()
	}
	return nil
}

func (m *memTier) Present(off, length int64) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for b := off / tierBlock; b <= (off+length-1)/tierBlock; b++ {
		if _, ok := m.blocks[b]; !ok {
			return false
		}
	}
	return off >= 0 && off+length <= m.size
}

func (m *memTier) ReadAt(p []byte, off int64) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for n := 0; n < len(p); {
		pos := off + int64(n)
		data, ok := m.blocks[pos/tierBlock]
		if !ok {
			return n, fmt.Errorf("block %d: %w", pos/tierBlock, source.ErrNotFound)
		}
		n += copy(p[n:], data[pos%tierBlock:])
	}
	return len(p), nil
}

func (m *memTier) Size() int64 {
	return m.size
}

func (m *memTier) Close() error {
	return nil
}

// slowTier is a slow backend of your own: a remote store with a round trip per request (here
// a source with a fixed delay in front of it). It implements source.Aborter, so a device
// that stops does not wait for reads blocked on the remote, and source.Identifier, so the
// COW file is pinned to the content version.
type slowTier struct {
	inner   source.Source
	delay   time.Duration
	version string
	abort   chan struct{}
	once    sync.Once
}

var (
	_ source.Aborter    = (*slowTier)(nil)
	_ source.Identifier = (*slowTier)(nil)
)

func newSlowTier(inner source.Source, delay time.Duration, version string) *slowTier {
	return &slowTier{inner: inner, delay: delay, version: version, abort: make(chan struct{})}
}

func (s *slowTier) ReadAt(p []byte, off int64) (int, error) {
	select {
	case <-time.After(s.delay): // the round trip
	case <-s.abort:
		return 0, errAborted
	}
	return s.inner.ReadAt(p, off)
}

func (s *slowTier) Abort() {
	s.once.Do(func() { close(s.abort) })
}

func (s *slowTier) Identity() string {
	return "remote:" + s.version + ":" + fmt.Sprint(s.inner.Size())
}

func (s *slowTier) Size() int64 {
	return s.inner.Size()
}

func (s *slowTier) Close() error {
	s.Abort()
	return s.inner.Close()
}

// tiered composes the base: the fast tier in front of the slow one, the slow one behind the
// block cache (1 MiB blocks, single-flight, sequential read-ahead), which a remote without
// a cache of its own wants. source.Cache tries the fast tier first and counts hits, misses
// and failures.
func tiered(fast *memTier, slow *slowTier) *source.Cache {
	return source.NewCache(fast, source.NewReadAhead(slow))
}
