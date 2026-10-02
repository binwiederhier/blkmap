package source

import (
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "f")
	require.NoError(t, os.WriteFile(path, pattern(1000), 0600))
	f, err := OpenFile(path, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(1000), f.Size())
	p := make([]byte, 10)
	n, err := f.ReadAt(p, 100)
	require.NoError(t, err)
	assert.Equal(t, 10, n)
	assert.Equal(t, pattern(1000)[100:110], p)
	n, err = f.ReadAt(p, 995)
	assert.ErrorIs(t, err, io.EOF)
	assert.Equal(t, 5, n)
	assert.Equal(t, pattern(1000)[995:], p[:5])
	require.NoError(t, f.Close())
}

func TestFileWindow(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "f")
	require.NoError(t, os.WriteFile(path, pattern(1000), 0600))
	f, err := OpenFile(path, 200, 300)
	require.NoError(t, err)
	assert.Equal(t, int64(300), f.Size())
	p := make([]byte, 10)
	n, err := f.ReadAt(p, 0)
	require.NoError(t, err)
	assert.Equal(t, 10, n)
	assert.Equal(t, pattern(1000)[200:210], p)
	n, err = f.ReadAt(p, 295)
	assert.ErrorIs(t, err, io.EOF)
	assert.Equal(t, 5, n)
	assert.Equal(t, pattern(1000)[495:500], p[:5])
	require.NoError(t, f.Close())
	// Offset only: size is the remainder
	f, err = OpenFile(path, 900, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(100), f.Size())
	require.NoError(t, f.Close())
}

func TestFileErrors(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "f")
	require.NoError(t, os.WriteFile(path, pattern(1000), 0600))
	_, err := OpenFile(filepath.Join(t.TempDir(), "missing"), 0, 0)
	require.Error(t, err)
	_, err = OpenFile(path, 1001, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "beyond")
	_, err = OpenFile(path, 500, 501)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "beyond")
}

// pattern returns n bytes of a non-repeating-looking test pattern.
func pattern(n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(i*7 + i/256)
	}
	return p
}

func TestFileHoles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sparse")
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(1<<20))
	_, err = f.WriteAt(pattern(64<<10), 512<<10) // data at 512K..576K, holes around it
	require.NoError(t, err)
	require.NoError(t, f.Close())
	src, err := OpenFile(path, 0, 0)
	require.NoError(t, err)
	holes, err := Holes(src, 0, 1<<20)
	require.NoError(t, err)
	// Filesystems round data extents to their block size; the hole edges must stay inside
	// the true holes and the data must not be reported as a hole
	require.Len(t, holes, 2)
	assert.Equal(t, int64(0), holes[0].Offset)
	assert.LessOrEqual(t, holes[0].Length, int64(512<<10))
	assert.GreaterOrEqual(t, holes[0].Length, int64(256<<10))
	assert.GreaterOrEqual(t, holes[1].Offset, int64(576<<10))
	assert.Equal(t, int64(1<<20), holes[1].Offset+holes[1].Length)
	// Windowed and clipped, relative to a source-offset
	win, err := OpenFile(path, 256<<10, 512<<10) // covers 256K..768K of the file
	require.NoError(t, err)
	holes, err = Holes(win, 0, 512<<10)
	require.NoError(t, err)
	require.Len(t, holes, 2)
	assert.Equal(t, Range{0, 256 << 10}, holes[0])
	assert.Equal(t, int64(512<<10), holes[1].Offset+holes[1].Length)
	holes, err = Holes(win, 260<<10, 60<<10) // file 516K..576K: entirely data
	require.NoError(t, err)
	assert.Empty(t, holes)
	require.NoError(t, src.Close())
	require.NoError(t, win.Close())
	// Concurrent Holes calls share one descriptor and must not corrupt each other
	src, err = OpenFile(path, 0, 0)
	require.NoError(t, err)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				h, err := Holes(src, 0, 1<<20)
				assert.NoError(t, err)
				assert.Len(t, h, 2)
			}
		}()
	}
	wg.Wait()
	require.NoError(t, src.Close())
}

func TestFileNoAlloc(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	require.NoError(t, os.WriteFile(path, pattern(1<<20), 0600))
	f, err := OpenFile(path, 0, 0)
	require.NoError(t, err)
	p := make([]byte, 4096)
	assert.Zero(t, testing.AllocsPerRun(100, func() { f.ReadAt(p, 8192) }))
	z := NewZero(1 << 20)
	assert.Zero(t, testing.AllocsPerRun(100, func() { z.ReadAt(p, 8192) }))
}
