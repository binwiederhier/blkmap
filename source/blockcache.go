package source

import (
	"container/list"
	"sync"
)

const (
	// cacheBlockSize is the unit a block cache fetches and keeps; every fetch is one block.
	cacheBlockSize = 1 << 20
	// cacheBlocks bounds the LRU (64 MiB per cache).
	cacheBlocks = 64
	// readAheadBlocks is how far a sequential reader triggers fetches past its current
	// block, and the bound on concurrent read-ahead fetches.
	readAheadBlocks = 8
)

// blockCache is an LRU of fixed-size blocks in front of a fetch function, with single-flight
// fetches (concurrent readers of a block share one fetch) and sequential read-ahead: a read
// that continues where the previous one ended starts fetching the following blocks in the
// background, so a reader that issues one request at a time still keeps the source busy.
type blockCache struct {
	fetch func(index int64) ([]byte, error)
	total int64

	blocks  map[int64]*list.Element
	lru     *list.List
	pending map[int64]*cacheFetch
	mu      sync.Mutex // Protects blocks, lru, pending, lastEnd

	lastEnd int64                  // where the previous read ended, for sequential detection
	ahead   chan struct{}          // semaphore bounding read-ahead goroutines
	skip    func(index int64) bool // blocks read-ahead must not fetch (holes), may be nil
}

// cacheBlock is one cached block.
type cacheBlock struct {
	index int64
	data  []byte
}

// cacheFetch is an in-flight fetch other readers can wait for.
type cacheFetch struct {
	done  chan struct{}
	block *cacheBlock
	err   error
}

func newBlockCache(total int64, fetch func(index int64) ([]byte, error)) *blockCache {
	return &blockCache{
		fetch:   fetch,
		total:   total,
		blocks:  make(map[int64]*list.Element),
		lru:     list.New(),
		pending: make(map[int64]*cacheFetch),
		lastEnd: -1,
		ahead:   make(chan struct{}, readAheadBlocks),
	}
}

// readAt copies [off, off+len(p)) of the resource into p through the cache. The caller
// keeps the range inside the resource.
func (c *blockCache) readAt(p []byte, off int64) error {
	end := off + int64(len(p))
	c.mu.Lock()
	sequential := off == c.lastEnd
	c.lastEnd = end
	c.mu.Unlock()
	for pos := off; pos < end; {
		block, err := c.block(pos / cacheBlockSize)
		if err != nil {
			return err
		}
		pos += int64(copy(p[pos-off:], block.data[pos%cacheBlockSize:]))
	}
	if sequential {
		c.readAhead((end - 1) / cacheBlockSize)
	}
	return nil
}

// block returns the block at index, fetching it on a miss; concurrent readers share one fetch.
func (c *blockCache) block(index int64) (*cacheBlock, error) {
	c.mu.Lock()
	if el, ok := c.blocks[index]; ok {
		c.lru.MoveToFront(el)
		c.mu.Unlock()
		return el.Value.(*cacheBlock), nil
	}
	if f, ok := c.pending[index]; ok {
		c.mu.Unlock()
		<-f.done
		return f.block, f.err
	}
	f := &cacheFetch{done: make(chan struct{})}
	c.pending[index] = f
	c.mu.Unlock()
	data, err := c.fetch(index)
	c.mu.Lock()
	delete(c.pending, index)
	if err != nil {
		f.err = err
	} else {
		f.block = &cacheBlock{index: index, data: data}
		c.blocks[index] = c.lru.PushFront(f.block)
		for c.lru.Len() > cacheBlocks {
			oldest := c.lru.Back()
			delete(c.blocks, oldest.Value.(*cacheBlock).index)
			c.lru.Remove(oldest)
		}
	}
	c.mu.Unlock()
	close(f.done)
	return f.block, f.err
}

// readAhead starts background fetches for the blocks after last, as many as the semaphore
// allows right now; blocks already cached or in flight cost nothing.
func (c *blockCache) readAhead(last int64) {
	for index := last + 1; index <= last+readAheadBlocks && index*cacheBlockSize < c.total; index++ {
		if c.skip != nil && c.skip(index) {
			continue
		}
		c.mu.Lock()
		_, cached := c.blocks[index]
		_, inflight := c.pending[index]
		c.mu.Unlock()
		if cached || inflight {
			continue
		}
		select {
		case c.ahead <- struct{}{}:
		default:
			return // enough read-ahead in flight already
		}
		go func(index int64) {
			defer func() { <-c.ahead }()
			c.block(index) // errors surface on the real read instead
		}(index)
	}
}
