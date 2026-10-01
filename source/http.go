package source

import (
	"container/list"
	"fmt"
	"io"
	"net/http"
	"sync"
)

const (
	// httpBlockSize is the aligned fetch unit; every Range request is one whole block.
	httpBlockSize = 1 << 20
	// httpCacheBlocks bounds the LRU block cache (64 MiB per HTTP source).
	httpCacheBlocks = 64
)

// HTTP reads a window of a URL via Range requests, through a small LRU block cache.
type HTTP struct {
	client *http.Client
	url    string
	offset int64
	size   int64
	total  int64 // length of the whole resource

	blocks map[int64]*list.Element // block index -> lru element holding *httpBlock
	lru    *list.List
	mu     sync.Mutex // Protects blocks and lru
}

// httpBlock is one cached, resource-aligned block.
type httpBlock struct {
	index int64
	data  []byte
}

// NewHTTP opens a window of size bytes starting at offset within the resource at url. A size
// of 0 means everything after offset. The server must support Range requests.
func NewHTTP(client *http.Client, url string, offset, size int64) (*HTTP, error) {
	resp, err := client.Head(url)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HEAD returned %d %s", url, resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	if resp.ContentLength < 0 {
		return nil, fmt.Errorf("%s: server does not report a Content-Length", url)
	}
	if size, err = window(offset, size, resp.ContentLength); err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	// Probe for Range support now rather than failing every read later with EIO (python's
	// http.server, for one, answers 200 with the whole body)
	if err := probeRange(client, url); err != nil {
		return nil, err
	}
	return &HTTP{
		client: client,
		url:    url,
		offset: offset,
		size:   size,
		total:  resp.ContentLength,
		blocks: make(map[int64]*list.Element),
		lru:    list.New(),
	}, nil
}

func (h *HTTP) ReadAt(p []byte, off int64) (int, error) {
	n, eof := clampRead(len(p), off, h.size)
	p = p[:n]
	// Walk the resource-aligned blocks the window read touches
	for pos := h.offset + off; len(p) > 0; {
		block, err := h.block(pos / httpBlockSize)
		if err != nil {
			return 0, err
		}
		copied := copy(p, block.data[pos%httpBlockSize:])
		p = p[copied:]
		pos += int64(copied)
	}
	return n, eof
}

func (h *HTTP) Size() int64 {
	return h.size
}

func (h *HTTP) Close() error {
	h.client.CloseIdleConnections()
	return nil
}

// block returns the cached block at index, fetching it on a miss.
func (h *HTTP) block(index int64) (*httpBlock, error) {
	h.mu.Lock()
	if el, ok := h.blocks[index]; ok {
		h.lru.MoveToFront(el)
		h.mu.Unlock()
		return el.Value.(*httpBlock), nil
	}
	h.mu.Unlock()
	data, err := h.fetch(index)
	if err != nil {
		return nil, err
	}
	block := &httpBlock{index: index, data: data}
	h.mu.Lock()
	defer h.mu.Unlock()
	if el, ok := h.blocks[index]; ok { // lost a race with another fetch of the same block
		return el.Value.(*httpBlock), nil
	}
	h.blocks[index] = h.lru.PushFront(block)
	for h.lru.Len() > httpCacheBlocks {
		oldest := h.lru.Back()
		delete(h.blocks, oldest.Value.(*httpBlock).index)
		h.lru.Remove(oldest)
	}
	return block, nil
}

// probeRange requests the first byte and insists on a 206 reply.
func probeRange(client *http.Client, url string) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("%s: server does not support Range requests (got %d for bytes=0-0)", url, resp.StatusCode)
	}
	return nil
}

// fetch issues one Range request for the whole block at index.
func (h *HTTP) fetch(index int64) ([]byte, error) {
	start := index * httpBlockSize
	end := min(start+httpBlockSize, h.total) - 1
	req, err := http.NewRequest(http.MethodGet, h.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("%s: expected 206 Partial Content for range %d-%d, got %d", h.url, start, end, resp.StatusCode)
	}
	data := make([]byte, end-start+1)
	if _, err := io.ReadFull(resp.Body, data); err != nil {
		return nil, fmt.Errorf("%s: short read for range %d-%d: %w", h.url, start, end, err)
	}
	return data, nil
}
