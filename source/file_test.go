package source

import (
	"io"
	"os"
	"path/filepath"
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
