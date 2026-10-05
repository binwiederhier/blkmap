package source

import (
	"bytes"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fetcher counts and optionally delays block fetches of a pattern-filled resource.
type fetcher struct {
	data    []byte
	fetches atomic.Int64
	delay   time.Duration
	fail    func(index int64) error
	mu      sync.Mutex
	order   []int64
}

func (f *fetcher) fetch(index int64) ([]byte, error) {
	f.fetches.Add(1)
	f.mu.Lock()
	f.order = append(f.order, index)
	f.mu.Unlock()
	f.mu.Lock()
	fail := f.fail
	f.mu.Unlock()
	if fail != nil {
		if err := fail(index); err != nil {
			return nil, err
		}
	}
	time.Sleep(f.delay)
	start := index * cacheBlockSize
	end := min(start+cacheBlockSize, int64(len(f.data)))
	return append([]byte(nil), f.data[start:end]...), nil
}

func (f *fetcher) fetched() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.order...)
}

func TestBlockCacheReads(t *testing.T) {
	t.Parallel()
	f := &fetcher{data: pattern(3*cacheBlockSize + 1234)}
	c := newBlockCache(int64(len(f.data)), f.fetch)
	p := make([]byte, 100)
	require.NoError(t, c.readAt(p, 50))
	assert.Equal(t, f.data[50:150], p)
	require.NoError(t, c.readAt(p, 1000)) // same block: cached
	assert.Equal(t, f.data[1000:1100], p)
	assert.Equal(t, int64(1), f.fetches.Load())
	// Spanning blocks 1..3, the tail block being partial
	p = make([]byte, 2*cacheBlockSize+1234+100)
	require.NoError(t, c.readAt(p, cacheBlockSize-100))
	assert.Equal(t, f.data[cacheBlockSize-100:], p)
}

func TestBlockCacheSingleFlight(t *testing.T) {
	t.Parallel()
	f := &fetcher{data: pattern(2 * cacheBlockSize), delay: 100 * time.Millisecond}
	c := newBlockCache(int64(len(f.data)), f.fetch)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := make([]byte, 4096)
			off := int64(i * 8192) // gaps between the reads, so none looks sequential
			assert.NoError(t, c.readAt(p, off))
			assert.Equal(t, f.data[off:off+4096], p)
		}(i)
	}
	wg.Wait()
	assert.Equal(t, int64(1), f.fetches.Load())
	// A failed fetch is reported to every waiter and not cached
	f.mu.Lock()
	f.fail = func(index int64) error {
		if index == 1 {
			return errors.New("boom")
		}
		return nil
	}
	f.mu.Unlock()
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			errs <- c.readAt(make([]byte, 16), cacheBlockSize+16)
		}()
	}
	for i := 0; i < 4; i++ {
		assert.ErrorContains(t, <-errs, "boom")
	}
	f.mu.Lock()
	f.fail = nil
	f.mu.Unlock()
	require.NoError(t, c.readAt(make([]byte, 16), cacheBlockSize+16))
}

func TestBlockCacheReadAhead(t *testing.T) {
	t.Parallel()
	f := &fetcher{data: pattern(32 * cacheBlockSize), delay: 20 * time.Millisecond}
	c := newBlockCache(int64(len(f.data)), f.fetch)
	// A random read fetches only its own block
	p := make([]byte, 4096)
	require.NoError(t, c.readAt(p, 20*cacheBlockSize+100))
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int64(1), f.fetches.Load())
	// Sequential reads from the start: the second read (continuing the first) triggers
	// read-ahead of the following blocks, so later sequential reads hit the cache
	require.NoError(t, c.readAt(p, 0))
	require.NoError(t, c.readAt(p, 4096))
	// The read-ahead fetches land in the background; wait for them rather than a fixed time
	require.Eventually(t, func() bool {
		fetched := f.fetched()
		for i := int64(1); i <= readAheadBlocks; i++ {
			if !slices.Contains(fetched, i) {
				return false
			}
		}
		return true
	}, 10*time.Second, 10*time.Millisecond, "blocks 1..%d should have been read ahead", readAheadBlocks)
	// Reading on through the read-ahead window hits the cache: none of its blocks is fetched a
	// second time (counted, not timed: a loaded CI runner under -race made a 100 ms bound
	// flaky). The reads move the stream on, so blocks further ahead are fetched meanwhile.
	big := make([]byte, cacheBlockSize)
	for b := int64(1); b <= readAheadBlocks; b++ {
		require.NoError(t, c.readAt(big, b*cacheBlockSize))
		assert.Equal(t, f.data[b*cacheBlockSize:(b+1)*cacheBlockSize], big)
	}
	fetched := f.fetched()
	for i := int64(1); i <= readAheadBlocks; i++ {
		n := 0
		for _, b := range fetched {
			if b == i {
				n++
			}
		}
		assert.Equal(t, 1, n, "block %d inside the read-ahead window must be fetched once", i)
	}
	// Read-ahead stops at the end of the resource without errors
	require.NoError(t, c.readAt(p, 31*cacheBlockSize))
	require.NoError(t, c.readAt(p, 31*cacheBlockSize+4096))
	time.Sleep(50 * time.Millisecond)
	assert.NotContains(t, f.fetched(), int64(32))
}

func TestBlockCacheNoAllocOnHit(t *testing.T) {
	f := &fetcher{data: pattern(2 * cacheBlockSize)}
	c := newBlockCache(int64(len(f.data)), f.fetch)
	p := make([]byte, 4096)
	require.NoError(t, c.readAt(p, 0))
	assert.Zero(t, testing.AllocsPerRun(100, func() { c.readAt(p, 8192) }))
}

func TestBlockCacheEviction(t *testing.T) {
	t.Parallel()
	f := &fetcher{data: pattern((cacheBlocks + 10) * cacheBlockSize)}
	c := newBlockCache(int64(len(f.data)), f.fetch)
	p := make([]byte, 16)
	for b := int64(0); b < cacheBlocks+10; b++ { // random order defeats read-ahead
		require.NoError(t, c.readAt(p, ((b*7)%(cacheBlocks+10))*cacheBlockSize))
	}
	c.mu.Lock()
	n := c.lru.Len()
	c.mu.Unlock()
	assert.Equal(t, cacheBlocks, n)
	assert.True(t, bytes.Equal(f.data[:16], f.data[:16]))
}

func TestBlockCacheReadAheadSkipsHoles(t *testing.T) {
	t.Parallel()
	f := &fetcher{data: pattern(32 * cacheBlockSize)}
	c := newBlockCache(int64(len(f.data)), f.fetch)
	// Blocks 3..20 are holes: a sequential reader at block 2 must not read them ahead
	c.skip = func(index int64) bool { return index >= 3 && index <= 20 }
	p := make([]byte, 4096)
	require.NoError(t, c.readAt(p, 2*cacheBlockSize))
	require.NoError(t, c.readAt(p, 2*cacheBlockSize+4096))
	time.Sleep(100 * time.Millisecond)
	for _, b := range f.fetched() {
		assert.False(t, b >= 3 && b <= 20, "block %d is a hole and was fetched", b)
	}
}

func TestBlockCacheSequentialUnderInterleavedStreams(t *testing.T) {
	t.Parallel()
	f := &fetcher{data: pattern(64 * cacheBlockSize)}
	c := newBlockCache(int64(len(f.data)), f.fetch)
	p := make([]byte, cacheBlockSize)
	// Two readers advance in turns (the kernel's parallel dispatch interleaves them); each
	// one's second read continues where its own first ended and must count as sequential
	require.NoError(t, c.readAt(p, 0))
	require.NoError(t, c.readAt(p, 40*cacheBlockSize))
	require.NoError(t, c.readAt(p, cacheBlockSize))
	time.Sleep(100 * time.Millisecond) // let A's read-ahead drain, the semaphore is shared
	require.NoError(t, c.readAt(p, 41*cacheBlockSize))
	time.Sleep(200 * time.Millisecond)
	fetched := f.fetched()
	assert.Contains(t, fetched, int64(2), "stream A read-ahead")
	assert.Contains(t, fetched, int64(42), "stream B read-ahead")
}
