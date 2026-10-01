package cow

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"heckel.io/blkmap/source"
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

func newTestStore(t *testing.T, dir string, base source.Source) *Store {
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

func TestStoreDiscard(t *testing.T) {
	t.Parallel()
	base := &mem{data: pattern(testSize)}
	dir := t.TempDir()
	s := newTestStore(t, dir, base)
	// Write chunks 2..5 fully, then discard a range covering 3 and 4 fully and 2 and 5 partially
	_, err := s.WriteAt(bytes.Repeat([]byte{'w'}, 4*testChunk), 2*testChunk)
	require.NoError(t, err)
	require.NoError(t, s.Discard(2*testChunk+100, 4*testChunk-200))
	expected := pattern(testSize)
	copy(expected[2*testChunk:], bytes.Repeat([]byte{'w'}, 4*testChunk))
	clear(expected[3*testChunk : 5*testChunk])
	assert.Equal(t, expected, readAll(t, s))
	assert.Equal(t, int64(4), s.Written()) // discarded chunks stay marked as written
	// Discarding never-written chunks changes nothing
	require.NoError(t, s.Discard(8*testChunk, 2*testChunk))
	assert.Equal(t, expected, readAll(t, s))
	assert.Equal(t, int64(4), s.Written())
	// The punched chunks no longer occupy disk blocks
	st, err := os.Stat(filepath.Join(dir, "d.cow"))
	require.NoError(t, err)
	assert.Equal(t, int64(2*testChunk), st.Sys().(*syscall.Stat_t).Blocks*512)
	// Out of range
	require.Error(t, s.Discard(testSize-100, 200))
	require.NoError(t, s.Close())
}

func TestStoreWriteZeroes(t *testing.T) {
	t.Parallel()
	base := &mem{data: pattern(testSize)}
	s := newTestStore(t, t.TempDir(), base)
	// Spans the tail of chunk 1, all of chunks 2 and 3, and the head of chunk 4
	require.NoError(t, s.WriteZeroes(2*testChunk-100, 2*testChunk+300))
	expected := pattern(testSize)
	clear(expected[2*testChunk-100 : 4*testChunk+200])
	assert.Equal(t, expected, readAll(t, s))
	assert.Equal(t, int64(4), s.Written())
	// Zeroing an already-written chunk
	_, err := s.WriteAt(bytes.Repeat([]byte{'q'}, testChunk), 6*testChunk)
	require.NoError(t, err)
	require.NoError(t, s.WriteZeroes(6*testChunk, testChunk))
	clear(expected[6*testChunk : 7*testChunk])
	assert.Equal(t, expected, readAll(t, s))
	require.Error(t, s.WriteZeroes(testSize-100, 200))
	require.NoError(t, s.Close())
}

// cached is a base whose plain reads return garbage and direct reads the real data, to
// tell the two hydration read paths apart.
type cached struct {
	mem
}

func (c *cached) ReadAt(p []byte, off int64) (int, error) {
	for i := range p {
		p[i] = 0xee
	}
	return len(p), nil
}

func (c *cached) ReadAtDirect(p []byte, off int64) (int, error) {
	return c.mem.ReadAt(p, off)
}

func TestStoreHydrateChunk(t *testing.T) {
	t.Parallel()
	base := &cached{mem: mem{data: pattern(testSize)}}
	dir := t.TempDir()
	s := newTestStore(t, dir, base)
	assert.Equal(t, int64(16), s.Chunks())
	assert.Equal(t, int64(testChunk), s.ChunkSize())
	// Through the cache path (plain reads)
	copied, err := s.HydrateChunk(3, false)
	require.NoError(t, err)
	assert.True(t, copied)
	assert.True(t, s.IsWritten(3))
	// Direct path
	copied, err = s.HydrateChunk(4, true)
	require.NoError(t, err)
	assert.True(t, copied)
	// Already written: no copy, content untouched
	_, err = s.WriteAt([]byte("guest"), 5*testChunk)
	require.NoError(t, err)
	copied, err = s.HydrateChunk(5, true)
	require.NoError(t, err)
	assert.False(t, copied)
	copied, err = s.HydrateChunk(3, true)
	require.NoError(t, err)
	assert.False(t, copied)
	// Plain reads (unwritten chunks, the chunk hydrated through the cache path, and the
	// read-modify-write around the guest write) all see the fake cache's fill byte; only
	// the directly hydrated chunk holds the real base data
	expected := bytes.Repeat([]byte{0xee}, testSize)
	copy(expected[4*testChunk:], pattern(testSize)[4*testChunk:5*testChunk])
	copy(expected[5*testChunk:], "guest")
	assert.Equal(t, expected, readAll(t, s))
	assert.Equal(t, int64(3), s.Written())
	// The last chunk may be partial
	short := &cached{mem: mem{data: pattern(testSize + 100)}}
	s2, err := Open(short, filepath.Join(dir, "p.cow"), filepath.Join(dir, "p.cow.bitmap"), testChunk)
	require.NoError(t, err)
	assert.Equal(t, int64(17), s2.Chunks())
	copied, err = s2.HydrateChunk(16, true)
	require.NoError(t, err)
	assert.True(t, copied)
	got := make([]byte, 100)
	_, err = s2.ReadAt(got, testSize)
	require.NoError(t, err)
	assert.Equal(t, pattern(testSize + 100)[testSize:], got)
	require.NoError(t, s2.Close())
	require.NoError(t, s.Close())
}

func TestStoreMarkZeroAndComplete(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bitmap := filepath.Join(dir, "d.cow.bitmap")
	_, _, complete, err := Complete(bitmap)
	require.NoError(t, err)
	assert.False(t, complete)
	s := newTestStore(t, dir, &mem{data: pattern(testSize)})
	assert.True(t, s.MarkZero(2))
	assert.False(t, s.MarkZero(2))
	assert.True(t, s.IsWritten(2))
	got := make([]byte, testChunk)
	_, err = s.ReadAt(got, 2*testChunk)
	require.NoError(t, err)
	assert.Equal(t, make([]byte, testChunk), got)
	for c := int64(0); c < s.Chunks(); c++ {
		s.MarkZero(c)
	}
	require.NoError(t, s.Close())
	size, chunkSize, complete, err := Complete(bitmap)
	require.NoError(t, err)
	assert.True(t, complete)
	assert.Equal(t, int64(testSize), size)
	assert.Equal(t, int64(testChunk), chunkSize)
	// Garbage is an error
	require.NoError(t, os.WriteFile(bitmap, []byte("junk"), 0600))
	_, _, _, err = Complete(bitmap)
	require.Error(t, err)
}
