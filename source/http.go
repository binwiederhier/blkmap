package source

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// httpBlockSize is the aligned fetch unit; every Range request is one whole block.
	httpBlockSize = cacheBlockSize
	// httpAttempts bounds retries of one block fetch on transient failures (network errors,
	// 5xx, truncated bodies); the delay doubles from httpRetryDelay between attempts.
	httpAttempts   = 3
	httpRetryDelay = 200 * time.Millisecond
	// httpProbeRange is the one-byte request that checks Range support and learns the size.
	httpProbeRange   = "bytes=0-0"
	httpContentRange = "Content-Range"
	httpRangeUnit    = "bytes "
)

// HTTP reads a window of a URL via Range requests, through the block cache (single-flight
// fetches, sequential read-ahead).
type HTTP struct {
	client *http.Client
	url    string
	offset int64
	size   int64
	total  int64 // length of the whole resource
	cache  *blockCache
}

// NewHTTP opens a window of size bytes starting at offset within the resource at url. A size
// of 0 means everything after offset. The server must support Range requests; a one-byte
// Range request checks that up front (python's http.server, for one, answers 200 with the
// whole body) and learns the size from Content-Range, so HEAD is never needed.
func NewHTTP(client *http.Client, url string, offset, size int64) (*HTTP, error) {
	total, err := probe(client, url)
	if err != nil {
		return nil, err
	}
	if size, err = window(offset, size, total); err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	h := &HTTP{client: client, url: url, offset: offset, size: size, total: total}
	h.cache = newBlockCache(total, h.fetch)
	return h, nil
}

// setMap lets the read-ahead skip blocks that the map says are holes; the Mapped wrapper
// around this source already never asks for them on demand.
func (h *HTTP) setMap(m *Map) {
	h.cache.skip = func(index int64) bool {
		start := index * httpBlockSize
		return len(m.Data(start, min(httpBlockSize, h.total-start))) == 0
	}
}

func (h *HTTP) ReadAt(p []byte, off int64) (int, error) {
	n, eof := clampRead(len(p), off, h.size)
	if err := h.cache.readAt(p[:n], h.offset+off); err != nil {
		return 0, err
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

// probe requests the first byte, insists on a 206 reply, and returns the resource size from
// Content-Range.
func probe(client *http.Client, url string) (int64, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", httpProbeRange)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return 0, fmt.Errorf("%s: server does not support Range requests (got 200 for %s)", url, httpProbeRange)
	}
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("%s: got %d %s", url, resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	// Content-Range: bytes 0-0/12345
	cr := resp.Header.Get(httpContentRange)
	total, err := strconv.ParseInt(cr[strings.LastIndex(cr, "/")+1:], 10, 64)
	if !strings.HasPrefix(cr, httpRangeUnit) || err != nil {
		return 0, fmt.Errorf("%s: cannot tell the size from Content-Range %q", url, cr)
	}
	return total, nil
}

// fetch issues the Range request for the whole block at index, retrying transient failures.
func (h *HTTP) fetch(index int64) ([]byte, error) {
	start := index * httpBlockSize
	end := min(start+httpBlockSize, h.total) - 1
	for attempt := 0; ; attempt++ {
		data, err, transient := h.fetchOnce(start, end)
		if err == nil || !transient || attempt == httpAttempts-1 {
			return data, err
		}
		time.Sleep(httpRetryDelay << attempt)
	}
}

// fetchOnce is one attempt; transient says whether a retry makes sense.
func (h *HTTP) fetchOnce(start, end int64) (data []byte, err error, transient bool) {
	req, err := http.NewRequest(http.MethodGet, h.url, nil)
	if err != nil {
		return nil, err, false
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: range %d-%d: %w", h.url, start, end, err), true
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusPartialContent:
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		return nil, fmt.Errorf("%s: range %d-%d: %d: %w", h.url, start, end, resp.StatusCode, ErrNotFound), false
	case resp.StatusCode >= 500:
		return nil, fmt.Errorf("%s: range %d-%d: got %d", h.url, start, end, resp.StatusCode), true
	default:
		return nil, fmt.Errorf("%s: expected 206 Partial Content for range %d-%d, got %d", h.url, start, end, resp.StatusCode), false
	}
	data = make([]byte, end-start+1)
	if _, err := io.ReadFull(resp.Body, data); err != nil {
		return nil, fmt.Errorf("%s: short read for range %d-%d: %w", h.url, start, end, err), true
	}
	return data, nil, false
}
