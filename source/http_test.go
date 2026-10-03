package source

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	// 8 readers of the same block at once: one fetch, every reader gets the data. The
	// offsets leave gaps so no read starts where another ended (that would look like a
	// sequential stream and start read-ahead of the next block)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := make([]byte, 4096)
			_, err := h.ReadAt(p, int64(i*8192))
			assert.NoError(t, err)
			assert.Equal(t, data[i*8192:i*8192+4096], p)
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
	// Concurrency is the property; elapsed time is only logged, since a loaded CI runner
	// makes any wall-clock bound flaky (serial would be 640 ms)
	assert.GreaterOrEqual(t, peak.Load(), int32(4))
}

func TestHTTPRejectsBogusContentRange(t *testing.T) {
	t.Parallel()
	for _, cr := range []string{"bytes 0-0/0", "bytes 0-0/-5", "bytes 0-0/*", "bytes 0-0/abc", "items 0-0/100"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Range", cr)
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte{0})
		}))
		_, err := NewHTTP(srv.Client(), srv.URL, 0, 0)
		assert.Error(t, err, cr)
		srv.Close()
	}
}

func TestHTTPVerifiesResponseRange(t *testing.T) {
	t.Parallel()
	data := pattern(2 * httpBlockSize)
	var lie atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if lie.Load() {
			// A broken origin answers 206 with the wrong range and a body of the right length
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", httpBlockSize-1, len(data)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(data[:httpBlockSize])
			return
		}
		http.ServeContent(w, r, "disk.img", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(srv.Close)
	h, err := NewHTTP(srv.Client(), srv.URL, 0, 0)
	require.NoError(t, err)
	lie.Store(true)
	p := make([]byte, 100)
	_, err = h.ReadAt(p, httpBlockSize)
	assert.Error(t, err, "a 206 for the wrong range must not be served as data")
}

func TestHTTPAbortUnblocksReads(t *testing.T) {
	t.Parallel()
	data := pattern(2 * httpBlockSize)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != httpProbeRange {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		http.ServeContent(w, r, "disk.img", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	h, err := NewHTTP(srv.Client(), srv.URL, 0, 0)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		_, err := h.ReadAt(make([]byte, 100), 0)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	h.Abort()
	select {
	case err := <-done:
		assert.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("read still blocked on the origin after Abort")
	}
	// Everything after an abort fails fast too, so a stopping device never waits on the origin
	_, err = h.ReadAt(make([]byte, 100), httpBlockSize)
	assert.Error(t, err)
}

func TestHTTPRedactsCredentials(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	url := strings.Replace(srv.URL, "http://", "http://user:secret@", 1)
	_, err := NewHTTP(srv.Client(), url, 0, 0)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "secret")
	assert.Contains(t, err.Error(), "user:xxxxx@")
}

func TestHTTPDetectsOriginChange(t *testing.T) {
	t.Parallel()
	data := pattern(4 * httpBlockSize)
	var etag atomic.Value
	etag.Store(`"v1"`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", etag.Load().(string))
		http.ServeContent(w, r, "img", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(srv.Close)
	h, err := NewHTTP(srv.Client(), srv.URL, 0, 0)
	require.NoError(t, err)
	_, err = h.ReadAt(make([]byte, 100), 0)
	require.NoError(t, err)
	// The image is replaced under a running device: mixing its blocks with the old ones
	// would corrupt the device, so reads fail instead
	etag.Store(`"v2"`)
	_, err = h.ReadAt(make([]byte, 100), 2*httpBlockSize)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "changed")
}

// TestHTTPVersionChange: the validator seen at open (ETag, else Last-Modified) is required
// on every fetch; an origin that changes the object, or stops sending the validator, must
// not have its new blocks mixed with the cached old ones.
func TestHTTPVersionChange(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"last_modified", "etag_disappears", "etag_changes"} {
		t.Run(mode, func(t *testing.T) {
			var version atomic.Int32
			version.Store(1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var a, b int64
				fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b)
				v := version.Load()
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, 8<<20))
				w.Header().Set("Last-Modified", time.Date(2026, 10, int(v), 0, 0, 0, 0, time.UTC).Format(http.TimeFormat))
				if mode == "etag_changes" || (mode == "etag_disappears" && v == 1) {
					w.Header().Set("ETag", fmt.Sprintf(`"v%d"`, v))
				}
				w.WriteHeader(http.StatusPartialContent)
				w.Write(bytes.Repeat([]byte{byte(v)}, int(b-a+1)))
			}))
			defer srv.Close()
			h, err := NewHTTP(srv.Client(), srv.URL, 0, 0)
			require.NoError(t, err)
			defer h.Close()
			old := make([]byte, 512)
			_, err = h.ReadAt(old, 0)
			require.NoError(t, err)
			version.Store(2)
			_, err = h.ReadAt(make([]byte, 512), 4<<20)
			assert.Error(t, err, "blocks of a changed object were accepted")
		})
	}
}
