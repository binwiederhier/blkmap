package source

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
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
	assert.Equal(t, int32(2), requests.Load()) // HEAD + Range probe
	// A read inside one block fetches that block once; a second read hits the cache
	p := make([]byte, 100)
	n, err := h.ReadAt(p, 50)
	require.NoError(t, err)
	assert.Equal(t, 100, n)
	assert.Equal(t, data[50:150], p)
	assert.Equal(t, int32(3), requests.Load())
	_, err = h.ReadAt(p, 1000)
	require.NoError(t, err)
	assert.Equal(t, data[1000:1100], p)
	assert.Equal(t, int32(3), requests.Load())
	// A read spanning four blocks, three of them new
	p = make([]byte, 2*httpBlockSize+10)
	n, err = h.ReadAt(p, httpBlockSize-5)
	require.NoError(t, err)
	assert.Equal(t, len(p), n)
	assert.Equal(t, data[httpBlockSize-5:httpBlockSize-5+len(p)], p)
	assert.Equal(t, int32(6), requests.Load())
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
