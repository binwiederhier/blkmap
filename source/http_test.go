package source

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTTP(t *testing.T) {
	t.Parallel()
	data := pattern(3*httpBlockSize + 1234)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.ServeContent(w, r, "disk.img", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(srv.Close)
	h, err := NewHTTP(srv.Client(), srv.URL+"/disk.img", 0, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), h.Size())
	assert.Equal(t, int32(1), requests.Load()) // the Range probe only, no HEAD
	// A read inside one block fetches that block once; a second read hits the cache
	p := make([]byte, 100)
	n, err := h.ReadAt(p, 50)
	require.NoError(t, err)
	assert.Equal(t, 100, n)
	assert.Equal(t, data[50:150], p)
	assert.Equal(t, int32(2), requests.Load())
	_, err = h.ReadAt(p, 1000)
	require.NoError(t, err)
	assert.Equal(t, data[1000:1100], p)
	assert.Equal(t, int32(2), requests.Load())
	// A read spanning four blocks, three of them new (non-sequential: no read-ahead)
	p = make([]byte, 2*httpBlockSize+10)
	n, err = h.ReadAt(p, httpBlockSize-5)
	require.NoError(t, err)
	assert.Equal(t, len(p), n)
	assert.Equal(t, data[httpBlockSize-5:httpBlockSize-5+len(p)], p)
	assert.Equal(t, int32(5), requests.Load())
	// Tail: short read + EOF
	p = make([]byte, 2000)
	n, err = h.ReadAt(p, int64(len(data))-1000)
	assert.ErrorIs(t, err, io.EOF)
	assert.Equal(t, 1000, n)
	assert.Equal(t, data[len(data)-1000:], p[:1000])
	require.NoError(t, h.Close())
}

func TestHTTPWindow(t *testing.T) {
	t.Parallel()
	data := pattern(5000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "disk.img", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(srv.Close)
	h, err := NewHTTP(srv.Client(), srv.URL, 1000, 2000)
	require.NoError(t, err)
	assert.Equal(t, int64(2000), h.Size())
	p := make([]byte, 10)
	_, err = h.ReadAt(p, 0)
	require.NoError(t, err)
	assert.Equal(t, data[1000:1010], p)
	n, err := h.ReadAt(p, 1995)
	assert.ErrorIs(t, err, io.EOF)
	assert.Equal(t, 5, n)
	assert.Equal(t, data[2995:3000], p[:5])
	h, err = NewHTTP(srv.Client(), srv.URL, 4000, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(1000), h.Size())
	_, err = NewHTTP(srv.Client(), srv.URL, 4000, 1001)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "beyond")
}

func TestHTTPNoRanges(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "5000")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	_, err := NewHTTP(srv.Client(), srv.URL, 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not support Range requests")
}

func TestHTTPErrors(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	_, err := NewHTTP(srv.Client(), srv.URL, 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
}

func TestHTTPSingleFlight(t *testing.T) {
	t.Parallel()
	data := pattern(2 * httpBlockSize)
	var gets atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.Header.Get("Range") != "bytes=0-0" {
			gets.Add(1)
			<-release // hold every block fetch until all readers are waiting
		}
		http.ServeContent(w, r, "disk.img", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(srv.Close)
	h, err := NewHTTP(srv.Client(), srv.URL, 0, 0)
	require.NoError(t, err)
	// 8 readers of the same block at once: one fetch, every reader gets the data
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := make([]byte, 4096)
			_, err := h.ReadAt(p, int64(i*4096))
			assert.NoError(t, err)
			assert.Equal(t, data[i*4096:(i+1)*4096], p)
		}(i)
	}
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
	assert.Equal(t, int32(1), gets.Load())
}

func TestHTTPRetriesTransientErrors(t *testing.T) {
	t.Parallel()
	data := pattern(httpBlockSize)
	var failures atomic.Int32
	failures.Store(2) // the first two block fetches fail with a 503
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.Header.Get("Range") != "bytes=0-0" {
			gets.Add(1)
			if failures.Add(-1) >= 0 {
				http.Error(w, "busy", http.StatusServiceUnavailable)
				return
			}
		}
		http.ServeContent(w, r, "disk.img", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(srv.Close)
	h, err := NewHTTP(srv.Client(), srv.URL, 0, 0)
	require.NoError(t, err)
	p := make([]byte, 4096)
	_, err = h.ReadAt(p, 0)
	require.NoError(t, err)
	assert.Equal(t, data[:4096], p)
	assert.Equal(t, int32(3), gets.Load())
	// A 404 is not transient: no retry, and the error says so
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=0-0" || r.Method == http.MethodHead {
			http.ServeContent(w, r, "disk.img", time.Time{}, bytes.NewReader(data))
			return
		}
		gets.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv2.Close)
	h2, err := NewHTTP(srv2.Client(), srv2.URL, 0, 0)
	require.NoError(t, err)
	gets.Store(0)
	_, err = h2.ReadAt(p, 0)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
	assert.Equal(t, int32(1), gets.Load())
}

func TestHTTPWithoutHead(t *testing.T) {
	t.Parallel()
	data := pattern(5000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		http.ServeContent(w, r, "disk.img", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(srv.Close)
	h, err := NewHTTP(srv.Client(), srv.URL, 1000, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(4000), h.Size())
	p := make([]byte, 10)
	_, err = h.ReadAt(p, 0)
	require.NoError(t, err)
	assert.Equal(t, data[1000:1010], p)
}

func TestHTTPReadAheadHonorsMap(t *testing.T) {
	t.Parallel()
	data := pattern(32 * httpBlockSize)
	var fetched []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.Header.Get("Range") != httpProbeRange {
			mu.Lock()
			fetched = append(fetched, r.Header.Get("Range"))
			mu.Unlock()
		}
		http.ServeContent(w, r, "disk.img", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(srv.Close)
	h, err := NewHTTP(srv.Client(), srv.URL, 0, 0)
	require.NoError(t, err)
	// Data only in blocks 0..1 and 20..21; everything else is a hole the map describes
	m, err := NewMap([]Range{{0, 2 * httpBlockSize}, {20 * httpBlockSize, 2 * httpBlockSize}})
	require.NoError(t, err)
	src := WithMap(h, m, 0)
	p := make([]byte, 64<<10)
	for off := int64(0); off < 2*httpBlockSize; off += int64(len(p)) { // sequential through the data
		_, err := src.ReadAt(p, off)
		require.NoError(t, err)
	}
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	for _, r := range fetched {
		var start, end int64
		fmt.Sscanf(r, "bytes=%d-%d", &start, &end)
		assert.True(t, end < 2*httpBlockSize || start >= 20*httpBlockSize, "range %s lies in a hole and was fetched by read-ahead", r)
	}
	assert.GreaterOrEqual(t, len(fetched), 2)
}

func TestHTTPSequentialReadAheadThroughput(t *testing.T) {
	t.Parallel()
	data := pattern(64 * httpBlockSize)
	var inflight, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != httpProbeRange {
			n := inflight.Add(1)
			for {
				m := peak.Load()
				if n <= m || peak.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			inflight.Add(-1)
		}
		http.ServeContent(w, r, "disk.img", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(srv.Close)
	h, err := NewHTTP(srv.Client(), srv.URL, 0, 0)
	require.NoError(t, err)
	// One synchronous reader, 1 MiB at a time: read-ahead must keep several fetches in
	// flight, so 32 reads take far less than 32 x 20 ms
	p := make([]byte, httpBlockSize)
	start := time.Now()
	for i := 0; i < 32; i++ {
		_, err := h.ReadAt(p, int64(i)*httpBlockSize)
		require.NoError(t, err)
		require.True(t, bytes.Equal(data[int64(i)*httpBlockSize:int64(i+1)*httpBlockSize], p), "block %d", i)
	}
	elapsed := time.Since(start)
	t.Logf("32 sequential 1 MiB reads over a 20 ms server: %s, peak %d in flight", elapsed, peak.Load())
	assert.Less(t, elapsed, 300*time.Millisecond)
	assert.GreaterOrEqual(t, peak.Load(), int32(4))
}
