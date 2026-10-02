package source

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	httpETag         = "ETag"
	httpLastModified = "Last-Modified"
	httpRangeUnit    = "bytes "
)

// HTTP reads a window of a URL via Range requests, through the block cache (single-flight
// fetches, sequential read-ahead).
type HTTP struct {
	client   *http.Client
	url      string
	name     string // url with the password hidden, for errors and logs
	offset   int64
	size     int64
	total    int64  // length of the whole resource
	etag     string // the resource version at open; every block must come from it
	modified string // Last-Modified, the version when there is no ETag
	cache    *blockCache
	ctx      context.Context // every request runs under it; Abort cancels it
	abort    context.CancelFunc
}

// NewHTTP opens a window of size bytes starting at offset within the resource at url. A size
// of 0 means everything after offset. The server must support Range requests; a one-byte
// Range request checks that up front (python's http.server, for one, answers 200 with the
// whole body) and learns the size from Content-Range, so HEAD is never needed.
func NewHTTP(client *http.Client, url string, offset, size int64) (*HTTP, error) {
	ctx, abort := context.WithCancel(context.Background())
	h := &HTTP{client: client, url: url, name: redact(url), offset: offset, ctx: ctx, abort: abort}
	total, err := h.probe()
	if err != nil {
		abort()
		return nil, err
	}
	if size, err = window(offset, size, total); err != nil {
		abort()
		return nil, fmt.Errorf("%s: %w", h.name, err)
	}
	h.size, h.total = size, total
	h.cache = newBlockCache(total, h.fetch)
	return h, nil
}

// setMap lets the read-ahead skip blocks that the map says are holes; the Mapped wrapper
// around this source already never asks for them on demand.
func (h *HTTP) setMap(m *Map) {
	h.cache.skip = func(index int64) bool {
		start := index * httpBlockSize
		return !m.HasData(start, min(httpBlockSize, h.total-start))
	}
}

func (h *HTTP) ReadAt(p []byte, off int64) (int, error) {
	n, eof := clampRead(len(p), off, h.size)
	if err := h.cache.readAt(p[:n], h.offset+off); err != nil {
		return 0, err
	}
	return n, eof
}

// Abort makes every in-flight and future fetch fail at once.
func (h *HTTP) Abort() {
	h.abort()
}

func (h *HTTP) Size() int64 {
	return h.size
}

func (h *HTTP) Close() error {
	h.abort()
	h.client.CloseIdleConnections()
	return nil
}

// probe requests the first byte, insists on a 206 reply, and returns the resource size from
// Content-Range.
func (h *HTTP) probe() (int64, error) {
	resp, err := h.get(0, 0)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return 0, fmt.Errorf("%s: server does not support Range requests (got 200 for %s)", h.name, httpProbeRange)
	}
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("%s: got %d %s", h.name, resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	start, _, total, err := parseContentRange(resp.Header.Get(httpContentRange))
	if err != nil || start != 0 {
		return 0, fmt.Errorf("%s: cannot tell the size from Content-Range %q", h.name, resp.Header.Get(httpContentRange))
	}
	h.etag, h.modified = resp.Header.Get(httpETag), resp.Header.Get(httpLastModified)
	return total, nil
}

// get issues the Range request for [start, end] under the source's context.
func (h *HTTP) get(start, end int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(h.ctx, http.MethodGet, h.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: range %d-%d: %w", h.name, start, end, err)
	}
	return resp, nil
}

// fetch issues the Range request for the whole block at index, retrying transient failures
// unless the source was aborted.
func (h *HTTP) fetch(index int64) ([]byte, error) {
	start := index * httpBlockSize
	end := min(start+httpBlockSize, h.total) - 1
	for attempt := 0; ; attempt++ {
		data, err, transient := h.fetchOnce(start, end)
		if err == nil || !transient || attempt == httpAttempts-1 || h.ctx.Err() != nil {
			return data, err
		}
		time.Sleep(httpRetryDelay << attempt)
	}
}

// fetchOnce is one attempt; transient says whether a retry makes sense. The reply must be a
// 206 for exactly the requested range: a server that answers with another range (or the
// whole file) would otherwise be served as data at the wrong offset.
func (h *HTTP) fetchOnce(start, end int64) (data []byte, err error, transient bool) {
	resp, err := h.get(start, end)
	if err != nil {
		return nil, err, true
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusPartialContent:
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		return nil, fmt.Errorf("%s: range %d-%d: %d: %w", h.name, start, end, resp.StatusCode, ErrNotFound), false
	case resp.StatusCode >= 500:
		return nil, fmt.Errorf("%s: range %d-%d: got %d", h.name, start, end, resp.StatusCode), true
	default:
		return nil, fmt.Errorf("%s: expected 206 Partial Content for range %d-%d, got %d", h.name, start, end, resp.StatusCode), false
	}
	if etag := resp.Header.Get(httpETag); h.etag != "" && etag != "" && etag != h.etag {
		return nil, fmt.Errorf("%s changed on the origin (ETag %s, was %s); refusing to mix its blocks with the old ones", h.name, etag, h.etag), false
	}
	if gotStart, gotEnd, total, err := parseContentRange(resp.Header.Get(httpContentRange)); err != nil || gotStart != start || gotEnd != end || total != h.total {
		return nil, fmt.Errorf("%s: asked for range %d-%d of %d, got Content-Range %q", h.name, start, end, h.total, resp.Header.Get(httpContentRange)), false
	}
	data = make([]byte, end-start+1)
	if _, err := io.ReadFull(resp.Body, data); err != nil {
		return nil, fmt.Errorf("%s: short read for range %d-%d: %w", h.name, start, end, err), true
	}
	return data, nil, false
}

// parseContentRange reads "bytes START-END/TOTAL" with a known, positive total.
func parseContentRange(cr string) (start, end, total int64, err error) {
	rest, ok := strings.CutPrefix(cr, httpRangeUnit)
	if !ok {
		return 0, 0, 0, fmt.Errorf("not a byte range: %q", cr)
	}
	span, totalStr, ok := strings.Cut(rest, "/")
	startStr, endStr, ok2 := strings.Cut(span, "-")
	if !ok || !ok2 {
		return 0, 0, 0, fmt.Errorf("malformed: %q", cr)
	}
	if start, err = strconv.ParseInt(startStr, 10, 64); err != nil {
		return 0, 0, 0, err
	}
	if end, err = strconv.ParseInt(endStr, 10, 64); err != nil {
		return 0, 0, 0, err
	}
	if total, err = strconv.ParseInt(totalStr, 10, 64); err != nil {
		return 0, 0, 0, err
	}
	if start < 0 || end < start || total <= end {
		return 0, 0, 0, fmt.Errorf("inconsistent: %q", cr)
	}
	return start, end, total, nil
}

// redact hides the password of a URL with credentials.
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Redacted()
}
