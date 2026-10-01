package cow

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testChunk = 4096
	testSize  = 16 * testChunk
)

// mem is an in-memory read-only base that counts reads.
type mem struct {
	data   []byte
	closed bool
	reads  int
	mu     sync.Mutex
}

func (m *mem) ReadAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	m.reads++
	m.mu.Unlock()
	if off >= int64(len(m.data)) {
		return 0, io.EOF
	}
	n := copy(p, m.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m *mem) Size() int64 {
	return int64(len(m.data))
}

func (m *mem) Close() error {
	m.closed = true
	return nil
}

func pattern(n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(i*7 + i/256)
	}
	return p
}

func newTestStore(t *testing.T, dir string, base *mem) *Store {
	t.Helper()
	s, err := Open(base, filepath.Join(dir, "d.cow"), filepath.Join(dir, "d.cow.bitmap"), testChunk)
	require.NoError(t, err)
	return s
}

func readAll(t *testing.T, s *Store) []byte {
	t.Helper()
	p := make([]byte, s.Size())
	n, err := s.ReadAt(p, 0)
	require.NoError(t, err)
	require.Equal(t, len(p), n)
	return p
}

func TestStoreReadThrough(t *testing.T) {
	t.Parallel()
	base := &mem{data: pattern(testSize)}
	s := newTestStore(t, t.TempDir(), base)
	assert.Equal(t, int64(testSize), s.Size())
	assert.Equal(t, int64(0), s.Written())
	assert.Equal(t, pattern(testSize), readAll(t, s))
	p := make([]byte, 100)
	n, err := s.ReadAt(p, testSize-50)
	assert.ErrorIs(t, err, io.EOF)
	assert.Equal(t, 50, n)
	require.NoError(t, s.Close())
	assert.True(t, base.closed)
}

func TestStoreWritePartialChunk(t *testing.T) {
	t.Parallel()
	base := &mem{data: pattern(testSize)}
	dir := t.TempDir()
	s := newTestStore(t, dir, base)
	// A 512-byte write in the middle of chunk 2: the rest of the chunk must still read as base
	n, err := s.WriteAt(bytes.Repeat([]byte{'x'}, 512), 2*testChunk+1024)
	require.NoError(t, err)
	assert.Equal(t, 512, n)
	assert.Equal(t, int64(1), s.Written())
	expected := pattern(testSize)
	copy(expected[2*testChunk+1024:], bytes.Repeat([]byte{'x'}, 512))
	assert.Equal(t, expected, readAll(t, s))
	// Chunk 2 now lives in the COW file: reading it does not touch base
	base.mu.Lock()
	before := base.reads
	base.mu.Unlock()
	_, err = s.ReadAt(make([]byte, testChunk), 2*testChunk)
	require.NoError(t, err)
	base.mu.Lock()
	assert.Equal(t, before, base.reads)
	base.mu.Unlock()
	// The COW file holds the full merged chunk at its device offset
	cow, err := os.ReadFile(filepath.Join(dir, "d.cow"))
	require.NoError(t, err)
	assert.Equal(t, expected[2*testChunk:3*testChunk], cow[2*testChunk:3*testChunk])
	require.NoError(t, s.Close())
}

func TestStoreWriteSpanning(t *testing.T) {
	t.Parallel()
	base := &mem{data: pattern(testSize)}
	s := newTestStore(t, t.TempDir(), base)
	// Spans the tail of chunk 0, all of chunk 1, and the head of chunk 2
	data := bytes.Repeat([]byte{'y'}, testChunk+200)
	n, err := s.WriteAt(data, testChunk-100)
	require.NoError(t, err)
	assert.Equal(t, len(data), n)
	assert.Equal(t, int64(3), s.Written())
	expected := pattern(testSize)
	copy(expected[testChunk-100:], data)
	assert.Equal(t, expected, readAll(t, s))
	// Overwrite inside an already-written chunk
	_, err = s.WriteAt([]byte("hello"), testChunk+10)
	require.NoError(t, err)
	copy(expected[testChunk+10:], "hello")
	assert.Equal(t, expected, readAll(t, s))
	assert.Equal(t, int64(3), s.Written())
	// Writes past the end are rejected
	_, err = s.WriteAt([]byte("x"), testSize)
	require.Error(t, err)
	n, err = s.WriteAt(make([]byte, 10), testSize-5)
	require.Error(t, err)
	assert.Equal(t, 0, n)
	require.NoError(t, s.Close())
}

func TestStorePersistence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newTestStore(t, dir, &mem{data: pattern(testSize)})
	_, err := s.WriteAt(bytes.Repeat([]byte{'z'}, 300), 5*testChunk+7)
	require.NoError(t, err)
	require.NoError(t, s.Flush())
	require.NoError(t, s.Close())
	// Reopen with a different base: unwritten chunks follow the new base, written ones stay
	other := bytes.Repeat([]byte{'B'}, testSize)
	s = newTestStore(t, dir, &mem{data: other})
	assert.Equal(t, int64(1), s.Written())
	expected := append([]byte{}, other...)
	copy(expected[5*testChunk:], pattern(testSize)[5*testChunk:6*testChunk])
	copy(expected[5*testChunk+7:], bytes.Repeat([]byte{'z'}, 300))
	assert.Equal(t, expected, readAll(t, s))
	require.NoError(t, s.Close())
}

func TestStoreConcurrentPartialWrites(t *testing.T) {
	t.Parallel()
	base := &mem{data: pattern(testSize)}
	s := newTestStore(t, t.TempDir(), base)
	// Many goroutines each write their own 64-byte slice of the same unwritten chunk; a
	// read-modify-write race would clobber a neighbour's bytes with base data
	const slots = testChunk / 64
	var wg sync.WaitGroup
	for i := 0; i < slots; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.WriteAt(bytes.Repeat([]byte{byte(i)}, 64), 3*testChunk+int64(i*64))
			assert.NoError(t, err)
		}(i)
	}
	wg.Wait()
	expected := pattern(testSize)
	for i := 0; i < slots; i++ {
		copy(expected[3*testChunk+i*64:], bytes.Repeat([]byte{byte(i)}, 64))
	}
	assert.Equal(t, expected, readAll(t, s))
	assert.Equal(t, int64(1), s.Written())
	require.NoError(t, s.Close())
}

func TestStoreGeometryMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newTestStore(t, dir, &mem{data: pattern(testSize)})
	require.NoError(t, s.Close())
	_, err := Open(&mem{data: pattern(2 * testSize)}, filepath.Join(dir, "d.cow"), filepath.Join(dir, "d.cow.bitmap"), testChunk)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "device size")
}
