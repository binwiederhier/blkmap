package source

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMap(t *testing.T) {
	t.Parallel()
	m, err := NewMap([]Range{{300, 100}, {0, 100}, {50, 100}, {400, 50}, {1000, 10}})
	require.NoError(t, err)
	assert.Equal(t, []Range{{0, 150}, {300, 150}, {1000, 10}}, m.Extents()) // merged, sorted
	assert.Equal(t, int64(310), m.DataBytes())
	assert.Equal(t, []Range{{150, 150}, {450, 550}}, m.Holes(0, 1010))
	assert.Equal(t, []Range{{450, 550}, {1010, 990}}, m.Holes(400, 1600)) // past the last extent
	assert.Equal(t, []Range{{160, 20}}, m.Holes(160, 20))
	assert.Empty(t, m.Holes(10, 20))
	assert.Equal(t, []Range{{10, 20}}, m.Data(10, 20))
	assert.Equal(t, []Range{{140, 10}, {300, 50}}, m.Data(140, 210))
	assert.Empty(t, m.Data(200, 50))
	for _, bad := range [][]Range{{{-1, 10}}, {{0, 0}}, {{0, -5}}} {
		_, err := NewMap(bad)
		require.Error(t, err, bad)
	}
	empty, err := NewMap(nil)
	require.NoError(t, err)
	assert.Equal(t, []Range{{0, 100}}, empty.Holes(0, 100))
	assert.Empty(t, empty.Data(0, 100))
}

func TestParseAndLoadMap(t *testing.T) {
	t.Parallel()
	m, err := ParseMap(strings.NewReader("# data extents\n0 1M\n4M 64K\n"))
	require.NoError(t, err)
	assert.Equal(t, []Range{{0, 1 << 20}, {4 << 20, 64 << 10}}, m.Extents())
	_, err = ParseMap(strings.NewReader("0 1M\n1M\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "line 2")
	dir := t.TempDir()
	path := filepath.Join(dir, "img.map")
	require.NoError(t, os.WriteFile(path, []byte("8K 4K\n"), 0600))
	m, err = LoadMap(path)
	require.NoError(t, err)
	assert.Equal(t, []Range{{8192, 4096}}, m.Extents())
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(srv.Close)
	m, err = LoadMap(srv.URL + "/img.map")
	require.NoError(t, err)
	assert.Equal(t, []Range{{8192, 4096}}, m.Extents())
	_, err = LoadMap(srv.URL + "/missing.map")
	require.Error(t, err)
	_, err = LoadMap(filepath.Join(dir, "missing.map"))
	require.Error(t, err)
}

func TestMapped(t *testing.T) {
	t.Parallel()
	// A 4 KiB resource whose only data is 1024..2048; the inner source holds garbage in the
	// holes, which must never be read nor shown
	inner := &recordingSource{mem: mem{data: bytes.Repeat([]byte{0xaa}, 4096)}}
	copy(inner.data[1024:2048], pattern(1024))
	m, err := NewMap([]Range{{1024, 1024}})
	require.NoError(t, err)
	w := WithMap(inner, m, 0)
	assert.Equal(t, int64(4096), w.Size())
	p := make([]byte, 4096)
	n, err := w.ReadAt(p, 0)
	require.NoError(t, err)
	assert.Equal(t, 4096, n)
	expected := make([]byte, 4096)
	copy(expected[1024:], pattern(1024))
	assert.Equal(t, expected, p)
	assert.Equal(t, []Range{{1024, 1024}}, inner.reads) // only the data was read
	// A read entirely in a hole touches nothing
	inner.reads = nil
	_, err = w.ReadAt(p[:100], 3000)
	require.NoError(t, err)
	assert.Equal(t, make([]byte, 100), p[:100])
	assert.Empty(t, inner.reads)
	holes, err := Holes(w, 0, 4096)
	require.NoError(t, err)
	assert.Equal(t, []Range{{0, 1024}, {2048, 2048}}, holes)
	// Direct reads bypass caches below but still honor the map
	inner.reads = nil
	_, err = ReadDirect(w, p[:2048], 512)
	require.NoError(t, err)
	assert.Equal(t, expected[512:2560], p[:2048])
	assert.Equal(t, []Range{{1024, 1024}}, inner.reads)
	require.NoError(t, w.Close())
	assert.True(t, inner.closed)
}

func TestMappedWithBase(t *testing.T) {
	t.Parallel()
	// The inner source is a window starting at resource offset 1M; the map is in resource
	// coordinates
	inner := &recordingSource{mem: mem{data: pattern(8192)}}
	m, err := NewMap([]Range{{1<<20 + 4096, 4096}}) // data at window offset 4096..8192
	require.NoError(t, err)
	w := WithMap(inner, m, 1<<20)
	p := make([]byte, 8192)
	_, err = w.ReadAt(p, 0)
	require.NoError(t, err)
	assert.Equal(t, make([]byte, 4096), p[:4096])
	assert.Equal(t, pattern(8192)[4096:], p[4096:])
	assert.Equal(t, []Range{{4096, 4096}}, inner.reads)
	holes, err := Holes(w, 0, 8192)
	require.NoError(t, err)
	assert.Equal(t, []Range{{0, 4096}}, holes)
}

func TestMappedNoAlloc(t *testing.T) {
	inner := &mem{data: pattern(1 << 20)}
	m, err := NewMap([]Range{{0, 256 << 10}, {512 << 10, 256 << 10}})
	require.NoError(t, err)
	w := WithMap(inner, m, 0)
	p := make([]byte, 4096)
	assert.Zero(t, testing.AllocsPerRun(100, func() { w.ReadAt(p, 100) }))         // data
	assert.Zero(t, testing.AllocsPerRun(100, func() { w.ReadAt(p, 300<<10) }))     // hole
	assert.Zero(t, testing.AllocsPerRun(100, func() { w.ReadAt(p, 256<<10-100) })) // straddling
}

func TestExtents(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sparse")
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(1<<20))
	_, err = f.WriteAt(pattern(64<<10), 512<<10)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	src, err := OpenFile(path, 0, 0)
	require.NoError(t, err)
	defer src.Close()
	ext, err := Extents(src)
	require.NoError(t, err)
	require.Len(t, ext, 1)
	assert.LessOrEqual(t, ext[0].Offset, int64(512<<10))
	assert.GreaterOrEqual(t, ext[0].Offset+ext[0].Length, int64(576<<10))
	assert.Less(t, ext[0].Length, int64(256<<10))
	ext, err = Extents(filled(100, 'x')) // cannot tell: all data
	require.NoError(t, err)
	assert.Equal(t, []Range{{0, 100}}, ext)
	ext, err = Extents(NewZero(100))
	require.NoError(t, err)
	assert.Empty(t, ext)
}

// recordingSource records the ranges read from it.
type recordingSource struct {
	mem
	reads []Range
}

func (r *recordingSource) ReadAt(p []byte, off int64) (int, error) {
	r.reads = append(r.reads, Range{off, int64(len(p))})
	return r.mem.ReadAt(p, off)
}
