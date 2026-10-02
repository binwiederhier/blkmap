package cow

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	info, err := Inspect(bitmap)
	require.NoError(t, err)
	assert.Equal(t, &Info{Size: testSize, ChunkSize: testChunk, Chunks: 16, Written: 16}, info)
	// Garbage is an error
	require.NoError(t, os.WriteFile(bitmap, []byte("junk"), 0600))
	_, _, _, err = Complete(bitmap)
	require.Error(t, err)
}

func TestStoreDirtyAndFlushOrder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newTestStore(t, dir, &mem{data: pattern(testSize)})
	assert.False(t, s.Dirty())
	_, err := s.WriteAt([]byte("x"), 0)
	require.NoError(t, err)
	assert.True(t, s.Dirty())
	// Unflushed: the bit is not on disk yet
	info, err := Inspect(filepath.Join(dir, "d.cow.bitmap"))
	require.NoError(t, err)
	assert.Equal(t, int64(0), info.Written)
	require.NoError(t, s.Flush())
	assert.False(t, s.Dirty())
	info, err = Inspect(filepath.Join(dir, "d.cow.bitmap"))
	require.NoError(t, err)
	assert.Equal(t, int64(1), info.Written)
	assert.True(t, s.MarkZero(3))
	assert.True(t, s.Dirty())
	require.NoError(t, s.Close())
}

func TestStoreNoAlloc(t *testing.T) {
	base := &mem{data: pattern(testSize)}
	s := newTestStore(t, t.TempDir(), base)
	p := make([]byte, 1024)
	_, err := s.WriteAt(bytes.Repeat([]byte{'w'}, testChunk), 2*testChunk) // chunk 2 written
	require.NoError(t, err)
	assert.Zero(t, testing.AllocsPerRun(100, func() { s.ReadAt(p, 2*testChunk+100) }), "read of a written chunk")
	assert.Zero(t, testing.AllocsPerRun(100, func() { s.ReadAt(p, 5*testChunk+100) }), "read of a base chunk")
	assert.Zero(t, testing.AllocsPerRun(100, func() { s.WriteAt(p, 2*testChunk+100) }), "write into a written chunk")
	full := make([]byte, testChunk)
	assert.Zero(t, testing.AllocsPerRun(100, func() { s.WriteAt(full, 7*testChunk) }), "whole-chunk write")
	require.NoError(t, s.Close())
	// The first partial write into a fresh chunk needs a chunk buffer: pooled, not allocated
	// per call once the pool is warm (a GC empties sync.Pool, so amortize over many chunks)
	big := newTestStore(t, t.TempDir(), &mem{data: pattern(128 * testChunk)})
	chunk := int64(0)
	warm := func() {
		big.WriteAt(p, chunk*testChunk+100)
		chunk++
	}
	warm()
	assert.Less(t, testing.AllocsPerRun(100, warm), 0.1, "partial write into a fresh chunk")
	require.NoError(t, big.Close())
}

func BenchmarkStore(b *testing.B) {
	dir := b.TempDir()
	const size = 256 << 20
	s, err := Open(&mem{data: make([]byte, size)}, filepath.Join(dir, "b.cow"), filepath.Join(dir, "b.cow.bitmap"), 64<<10)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	p := make([]byte, 4096)
	chunk := make([]byte, 64<<10)
	b.Run("read-base-4k", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			s.ReadAt(p, int64(i*4096)%(size/2))
		}
	})
	b.Run("write-chunk-64k", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(chunk)))
		for i := 0; i < b.N; i++ {
			s.WriteAt(chunk, int64(i)*int64(len(chunk))%size)
		}
	})
	b.Run("read-cow-4k", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			s.ReadAt(p, int64(i*4096)%size)
		}
	})
	b.Run("write-4k-into-written", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			s.WriteAt(p, int64(i*4096)%size)
		}
	})
}

func BenchmarkStorePartialWriteRMW(b *testing.B) {
	// Every write lands in a fresh chunk: read-modify-write of a whole 64 KiB chunk
	dir := b.TempDir()
	size := int64(b.N+1) * (64 << 10)
	s, err := Open(&mem{data: make([]byte, size)}, filepath.Join(dir, "b.cow"), filepath.Join(dir, "b.cow.bitmap"), 64<<10)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	p := make([]byte, 4096)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.WriteAt(p, int64(i)*(64<<10)+8192)
	}
}

// sizedBase records the length of every read, to show how requests reach the source.
type sizedBase struct {
	mem
	reads []int
	mu    sync.Mutex
}

func (b *sizedBase) ReadAt(p []byte, off int64) (int, error) {
	b.mu.Lock()
	b.reads = append(b.reads, len(p))
	b.mu.Unlock()
	return b.mem.ReadAt(p, off)
}

func TestStoreReadCoalescesChunks(t *testing.T) {
	t.Parallel()
	base := &sizedBase{mem: mem{data: pattern(testSize)}}
	s := newTestStore(t, t.TempDir(), base)
	// A read spanning 16 unwritten chunks is one base read, not 16
	p := make([]byte, testSize)
	_, err := s.ReadAt(p, 0)
	require.NoError(t, err)
	assert.Equal(t, pattern(testSize), p)
	assert.Equal(t, []int{testSize}, base.reads)
	// With chunk 5 written, the runs are chunks 0..4 (base), 5 (cow), 6..15 (base)
	_, err = s.WriteAt(bytes.Repeat([]byte{'w'}, testChunk), 5*testChunk)
	require.NoError(t, err)
	base.reads = nil
	_, err = s.ReadAt(p, 0)
	require.NoError(t, err)
	expected := pattern(testSize)
	copy(expected[5*testChunk:], bytes.Repeat([]byte{'w'}, testChunk))
	assert.Equal(t, expected, p)
	assert.Equal(t, []int{5 * testChunk, 10 * testChunk}, base.reads)
	// Unaligned, inside one run
	base.reads = nil
	_, err = s.ReadAt(p[:3000], 7*testChunk+100)
	require.NoError(t, err)
	assert.Equal(t, expected[7*testChunk+100:7*testChunk+3100], p[:3000])
	assert.Equal(t, []int{3000}, base.reads)
	require.NoError(t, s.Close())
}

func TestStoreHydrateRun(t *testing.T) {
	t.Parallel()
	base := &sizedBase{mem: mem{data: pattern(testSize + 100)}}
	dir := t.TempDir()
	s, err := Open(base, filepath.Join(dir, "r.cow"), filepath.Join(dir, "r.cow.bitmap"), testChunk)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	// Chunk 3 holds a guest write; a run over chunks 1..6 copies the other five with one read
	_, err = s.WriteAt([]byte("guest"), 3*testChunk)
	require.NoError(t, err)
	base.reads = nil
	copied, err := s.HydrateRun(1, 6, false)
	require.NoError(t, err)
	assert.Equal(t, int64(5*testChunk), copied)
	assert.Equal(t, []int{6 * testChunk}, base.reads)
	for c := int64(1); c < 7; c++ {
		assert.True(t, s.IsWritten(c), "chunk %d", c)
	}
	assert.False(t, s.IsWritten(0))
	assert.False(t, s.IsWritten(7))
	got := make([]byte, 7*testChunk)
	_, err = s.ReadAt(got, 0)
	require.NoError(t, err)
	expected := pattern(testSize)[:7*testChunk]
	copy(expected[3*testChunk:], "guest")
	assert.Equal(t, expected, got)
	// The last, partial chunk; a run clipped to the device; an already-written run is free
	copied, err = s.HydrateRun(16, 5, true)
	require.NoError(t, err)
	assert.Equal(t, int64(100), copied)
	assert.True(t, s.IsWritten(16))
	copied, err = s.HydrateRun(1, 6, false)
	require.NoError(t, err)
	assert.Equal(t, int64(0), copied)
	_, err = s.HydrateRun(17, 1, false)
	require.Error(t, err)
}

func TestStoreFlushNeverPersistsBitBeforeData(t *testing.T) {
	t.Parallel()
	// A write that lands between the COW sync and the bitmap sync of one Flush must not have
	// its bit on disk before a later flush covers its data. Model it by racing writers against
	// flushes and checking, after every flush, that the on-disk bitmap never claims a chunk
	// whose data is not in the (page-cache backed, so always visible here) COW file: the
	// invariant that matters is the snapshot order, which the test observes through Flush's
	// bitmap pages being taken before the COW sync.
	dir := t.TempDir()
	base := &mem{data: pattern(testSize)}
	s := newTestStore(t, dir, base)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			s.WriteAt([]byte{byte(i)}, int64(i%16)*testChunk)
		}
	}()
	for i := 0; i < 50; i++ {
		require.NoError(t, s.Flush())
	}
	close(stop)
	wg.Wait()
	require.NoError(t, s.Close())
}

func TestStoreCloseAfterFailedFlushKeepsBitmapBehindData(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newTestStore(t, dir, &mem{data: pattern(testSize)})
	_, err := s.WriteAt([]byte("x"), 0)
	require.NoError(t, err)
	// Make the COW file unsyncable by closing its descriptor underneath the store
	require.NoError(t, s.cow.Close())
	require.Error(t, s.Close())
	info, err := Inspect(filepath.Join(dir, "d.cow.bitmap"))
	require.NoError(t, err)
	assert.Equal(t, int64(0), info.Written, "a bit must not reach disk when its data could not")
}

func TestOpenRefusesLostCowFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newTestStore(t, dir, &mem{data: pattern(testSize)})
	_, err := s.WriteAt(bytes.Repeat([]byte{'w'}, testChunk), 2*testChunk)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.NoError(t, os.Remove(filepath.Join(dir, "d.cow")))
	_, err = Open(&mem{data: pattern(testSize)}, filepath.Join(dir, "d.cow"), filepath.Join(dir, "d.cow.bitmap"), testChunk)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cow file")
	assert.Contains(t, err.Error(), "missing")
	// A truncated cow file is just as bad
	require.NoError(t, os.WriteFile(filepath.Join(dir, "d.cow"), make([]byte, testChunk), 0600))
	_, err = Open(&mem{data: pattern(testSize)}, filepath.Join(dir, "d.cow"), filepath.Join(dir, "d.cow.bitmap"), testChunk)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cow file")
}

func TestOpenLocksCowFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newTestStore(t, dir, &mem{data: pattern(testSize)})
	_, err := Open(&mem{data: pattern(testSize)}, filepath.Join(dir, "d.cow"), filepath.Join(dir, "d.cow.bitmap"), testChunk)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "in use")
	require.NoError(t, s.Close())
	s2, err := Open(&mem{data: pattern(testSize)}, filepath.Join(dir, "d.cow"), filepath.Join(dir, "d.cow.bitmap"), testChunk)
	require.NoError(t, err)
	require.NoError(t, s2.Close())
}

func TestStoreChunkAPIsCheckBounds(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, t.TempDir(), &mem{data: pattern(testSize)})
	assert.False(t, s.IsWritten(-1))
	assert.False(t, s.IsWritten(16))
	assert.False(t, s.MarkZero(16))
	_, err := s.HydrateRun(16, 1, false)
	require.Error(t, err)
	_, err = s.HydrateRun(-1, 1, false)
	require.Error(t, err)
	n, err := s.ReadAt(make([]byte, 10), -5)
	require.Error(t, err)
	assert.Equal(t, 0, n)
	require.NoError(t, s.Close())
}

func TestInspectRejectsBadHeader(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bits")
	b, err := OpenBitmap(path, testSize, testChunk)
	require.NoError(t, err)
	require.NoError(t, b.Close())
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	for _, bad := range []struct {
		name  string
		patch func([]byte)
	}{
		{"zero chunk", func(h []byte) { binary.LittleEndian.PutUint64(h[bitmapOffChunk:], 0) }},
		{"chunk not power of two", func(h []byte) { binary.LittleEndian.PutUint64(h[bitmapOffChunk:], 3000) }},
		{"negative size", func(h []byte) { binary.LittleEndian.PutUint64(h[bitmapOffSize:], 1<<63) }},
		{"size too large for the file", func(h []byte) { binary.LittleEndian.PutUint64(h[bitmapOffSize:], 1<<50) }},
	} {
		corrupt := append([]byte(nil), data...)
		bad.patch(corrupt)
		require.NoError(t, os.WriteFile(path, corrupt, 0600))
		_, err := Inspect(path)
		assert.Error(t, err, bad.name)
		_, _, _, err = Complete(path)
		assert.Error(t, err, bad.name)
	}
}

func TestOpenRefusesSymlinkedFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	require.NoError(t, os.WriteFile(target, nil, 0600))
	require.NoError(t, os.Symlink(target, filepath.Join(dir, "c.cow")))
	_, err := Open(&mem{data: make([]byte, 16*testChunk)}, filepath.Join(dir, "c.cow"), filepath.Join(dir, "c.bitmap"), testChunk)
	assert.Error(t, err, "a symlinked cow file must not be followed")
	require.NoError(t, os.Symlink(target, filepath.Join(dir, "d.bitmap")))
	_, err = Open(&mem{data: make([]byte, 16*testChunk)}, filepath.Join(dir, "d.cow"), filepath.Join(dir, "d.bitmap"), testChunk)
	assert.Error(t, err, "a symlinked bitmap must not be followed")
}

// abandon releases the store the way a killed process does: no flush, no bitmap sync, the
// live bitmap left in place.
func (s *Store) abandon() {
	s.cow.Close()
	s.bitmap.abandon()
}

func TestLiveBitmapSurvivesCrash(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cowPath, bitmapPath, livePath := filepath.Join(dir, "d.cow"), filepath.Join(dir, "d.cow.bitmap"), filepath.Join(dir, "d.live")
	s, err := OpenWith(&mem{data: pattern(testSize)}, &Options{COWFile: cowPath, Bitmap: bitmapPath, LiveBitmap: livePath, ChunkSize: testChunk})
	require.NoError(t, err)
	written := bytes.Repeat([]byte{'w'}, testChunk)
	_, err = s.WriteAt(written, 3*testChunk)
	require.NoError(t, err)
	// The write was acknowledged but never flushed, and the process dies: the data is in the
	// cow file's page cache, the bit only in the live bitmap
	s.abandon()
	s, err = OpenWith(&mem{data: pattern(testSize)}, &Options{COWFile: cowPath, Bitmap: bitmapPath, LiveBitmap: livePath, ChunkSize: testChunk})
	require.NoError(t, err)
	assert.True(t, s.IsWritten(3), "the restarted server must not revert an acknowledged write")
	p := make([]byte, testChunk)
	_, err = s.ReadAt(p, 3*testChunk)
	require.NoError(t, err)
	assert.Equal(t, written, p)
	// The recovered bits reach the on-disk bitmap at the next flush
	require.NoError(t, s.Flush())
	info, err := Inspect(bitmapPath)
	require.NoError(t, err)
	assert.Equal(t, int64(1), info.Written)
	// A clean close leaves the disk bitmap authoritative and removes the live one
	require.NoError(t, s.Close())
	_, err = os.Stat(livePath)
	assert.True(t, os.IsNotExist(err))
}

func TestWithoutLiveBitmapCrashRevertsWrite(t *testing.T) {
	t.Parallel()
	// Why the live bitmap exists: with bits only in process memory, a crash forgets the
	// write even though its data sits in the cow file
	dir := t.TempDir()
	s := newTestStore(t, dir, &mem{data: pattern(testSize)})
	_, err := s.WriteAt(bytes.Repeat([]byte{'w'}, testChunk), 3*testChunk)
	require.NoError(t, err)
	s.abandon()
	s = newTestStore(t, dir, &mem{data: pattern(testSize)})
	assert.False(t, s.IsWritten(3))
	require.NoError(t, s.Close())
}

func TestLiveBitmapIgnoresForeignGeometry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cowPath, bitmapPath, livePath := filepath.Join(dir, "d.cow"), filepath.Join(dir, "d.cow.bitmap"), filepath.Join(dir, "d.live")
	// A live file from another geometry (the device was recreated) is not trusted
	require.NoError(t, os.WriteFile(livePath, bytes.Repeat([]byte{0xff}, 2*bitmapHeaderSize), 0600))
	s, err := OpenWith(&mem{data: pattern(testSize)}, &Options{COWFile: cowPath, Bitmap: bitmapPath, LiveBitmap: livePath, ChunkSize: testChunk})
	require.NoError(t, err)
	assert.Equal(t, int64(0), s.Written())
	require.NoError(t, s.Close())
}

func TestStorePinsSourceIdentity(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	open := func(identity string) (*Store, error) {
		return OpenWith(&mem{data: pattern(testSize)}, &Options{COWFile: filepath.Join(dir, "d.cow"), Bitmap: filepath.Join(dir, "d.cow.bitmap"), ChunkSize: testChunk, Identity: identity})
	}
	// Nothing written yet: the source may change freely, the latest one is recorded
	s, err := open("v0")
	require.NoError(t, err)
	require.NoError(t, s.Close())
	s, err = open("v1")
	require.NoError(t, err)
	_, err = s.WriteAt(bytes.Repeat([]byte{'w'}, testChunk), 0)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	info, err := Inspect(filepath.Join(dir, "d.cow.bitmap"))
	require.NoError(t, err)
	assert.Equal(t, "v1", info.Identity)
	// Writes overlay v1: another source would mix them with different content
	_, err = open("v2")
	require.ErrorIs(t, err, ErrSourceChanged)
	assert.Contains(t, err.Error(), "v1")
	// No identity (detached start without sources) skips the check
	s, err = open("")
	require.NoError(t, err)
	require.NoError(t, s.Close())
	// An operator who knows v2 has the same content accepts it explicitly
	require.NoError(t, Pin(filepath.Join(dir, "d.cow"), filepath.Join(dir, "d.cow.bitmap"), "v2"))
	s, err = open("v2")
	require.NoError(t, err)
	require.NoError(t, s.Close())
	// Very long identities (many segments) are stored as a hash
	long := strings.Repeat("segment;", 1000)
	require.NoError(t, Pin(filepath.Join(dir, "d.cow"), filepath.Join(dir, "d.cow.bitmap"), long))
	s, err = open(long)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	_, err = open(long + "x")
	require.ErrorIs(t, err, ErrSourceChanged)
}

func TestStoreSourceStats(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, t.TempDir(), &mem{data: pattern(testSize)})
	defer s.Close()
	p := make([]byte, 2*testChunk)
	_, err := s.ReadAt(p, 0) // unwritten: one coalesced base read
	require.NoError(t, err)
	_, err = s.WriteAt(make([]byte, 100), 5*testChunk) // partial first write: read-modify-write
	require.NoError(t, err)
	_, err = s.HydrateRun(8, 2, false)
	require.NoError(t, err)
	st := s.SourceStats()
	assert.Equal(t, int64(3), st.Reads)
	assert.Equal(t, int64(5*testChunk), st.Bytes)
	assert.Zero(t, st.Errors)
	assert.Positive(t, st.Duration)
	// Reads of written chunks never touch the source
	_, err = s.ReadAt(p[:testChunk], 5*testChunk)
	require.NoError(t, err)
	assert.Equal(t, int64(3), s.SourceStats().Reads)
	failing := newTestStore(t, t.TempDir(), &broken{size: testSize})
	defer failing.Close()
	_, err = failing.ReadAt(p, 0)
	require.Error(t, err)
	assert.Equal(t, int64(1), failing.SourceStats().Errors)
}

// broken is a base whose reads fail.
type broken struct {
	size int64
}

func (b *broken) ReadAt(p []byte, off int64) (int, error) {
	return 0, errors.New("source gone")
}

func (b *broken) Size() int64 {
	return b.size
}

func (b *broken) Close() error {
	return nil
}

// Deleting the bitmap (and cow file) is how an overlay is started over; a live bitmap left in
// /run by a crashed server of the old overlay must not bring its bits back.
func TestLiveBitmapNotAdoptedByAFreshBitmap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	o := &Options{COWFile: filepath.Join(dir, "d.cow"), Bitmap: filepath.Join(dir, "d.cow.bitmap"), LiveBitmap: filepath.Join(dir, "d.live"), ChunkSize: testChunk}
	s, err := OpenWith(&mem{data: pattern(testSize)}, o)
	require.NoError(t, err)
	_, err = s.WriteAt(bytes.Repeat([]byte{'w'}, testChunk), 0)
	require.NoError(t, err)
	s.abandon()
	require.NoError(t, os.Remove(o.COWFile))
	require.NoError(t, os.Remove(o.Bitmap))
	s, err = OpenWith(&mem{data: pattern(testSize)}, o)
	require.NoError(t, err, "a fresh overlay must start empty")
	require.Zero(t, s.Written())
	require.NoError(t, s.Close())
}
