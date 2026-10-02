package cow

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

type memWriter struct{ b []byte }

func (m *memWriter) WriteAt(p []byte, off int64) (int, error) {
	copy(m.b[off:], p)
	return len(p), nil
}

// Writeback commits exactly the written chunks: the result equals what the device reads, and
// untouched chunks of the destination stay as they were.
func TestWriteback(t *testing.T) {
	const chunk = 4096
	base := make([]byte, 10*chunk+100)
	for i := range base {
		base[i] = byte(i)
	}
	dir := t.TempDir()
	s, err := Open(&mem{data: bytes.Clone(base)}, filepath.Join(dir, "cow"), filepath.Join(dir, "bitmap"), chunk)
	require.NoError(t, err)
	defer s.Close()
	_, err = s.WriteAt(bytes.Repeat([]byte{0xAA}, 100), chunk+50) // partial chunk 1
	require.NoError(t, err)
	_, err = s.WriteAt(bytes.Repeat([]byte{0xBB}, 2*chunk), 5*chunk) // chunks 5 and 6
	require.NoError(t, err)
	_, err = s.WriteAt([]byte{1, 2, 3}, 10*chunk+97) // the short last chunk
	require.NoError(t, err)
	require.NoError(t, s.Discard(5*chunk, chunk)) // chunk 5 punched: reads as zeros
	dst := &memWriter{b: bytes.Clone(base)}
	n, err := s.Writeback(dst)
	require.NoError(t, err)
	require.Equal(t, int64(4), n)
	want := make([]byte, len(base))
	_, err = s.ReadAt(want, 0)
	require.NoError(t, err)
	require.Equal(t, want, dst.b)
	require.Equal(t, base[:chunk], dst.b[:chunk], "untouched chunk 0 kept")
	require.Equal(t, bytes.Repeat([]byte{0}, chunk), dst.b[5*chunk:6*chunk], "punched chunk written as zeros")
}
