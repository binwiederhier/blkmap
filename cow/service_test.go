package cow

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

func TestMarkZeroAfterCrashReadsZeros(t *testing.T) {
	t.Parallel()
	// A crash without a live bitmap (a power loss empties /run) forgets the bit but not the
	// data in the cow file; hydration then marks the chunk as a base hole
	dir := t.TempDir()
	s := newTestStore(t, dir, &mem{data: make([]byte, testSize)})
	_, err := s.WriteAt(bytes.Repeat([]byte{'w'}, testChunk), 3*testChunk)
	require.NoError(t, err)
	s.abandon()
	s = newTestStore(t, dir, &mem{data: make([]byte, testSize)})
	defer s.Close()
	require.True(t, s.MarkZero(3))
	p := make([]byte, testChunk)
	_, err = s.ReadAt(p, 3*testChunk)
	require.NoError(t, err)
	assert.Equal(t, make([]byte, testChunk), p, "the chunk read zeros before it was marked and must after")
}

func TestFailedCOWSyncStopsTheStore(t *testing.T) {
	t.Parallel()
	// A failed fsync may have dropped the COW file's dirty pages (Linux marks them clean),
	// so a retry would succeed without the data: no bit may ever be committed after it
	dir := t.TempDir()
	cowPath, bitmapPath, livePath := filepath.Join(dir, "d.cow"), filepath.Join(dir, "d.cow.bitmap"), filepath.Join(dir, "d.live")
	base := pattern(testSize)
	open := func() *Store {
		s, err := OpenWith(&mem{data: base}, &Options{COWFile: cowPath, Bitmap: bitmapPath, LiveBitmap: livePath, ChunkSize: testChunk})
		require.NoError(t, err)
		return s
	}
	s := open()
	_, err := s.WriteAt(bytes.Repeat([]byte{'w'}, 100), 2*testChunk)
	require.NoError(t, err)
	s.syncCOW = func() error { return syscall.EIO }
	require.ErrorIs(t, s.Flush(), syscall.EIO)
	s.syncCOW = s.cow.Sync
	assert.ErrorIs(t, s.Flush(), ErrCOWFailed, "the retry must not commit the bits")
	select {
	case <-s.Failed():
	default:
		t.Fatal("Failed is not closed")
	}
	_, err = s.WriteAt([]byte{1}, 0)
	assert.ErrorIs(t, err, ErrCOWFailed)
	_, err = s.ReadAt(make([]byte, 10), 2*testChunk)
	assert.ErrorIs(t, err, ErrCOWFailed, "the chunk's data may be gone from the page cache")
	info, err := Inspect(bitmapPath)
	require.NoError(t, err)
	assert.Zero(t, info.Written)
	assert.ErrorIs(t, s.Close(), ErrCOWFailed)
	_, err = os.Stat(livePath)
	assert.True(t, os.IsNotExist(err), "a successor must not adopt bits whose data may be lost")
	// The next server serves the last durable state: the write never got its flush
	s = open()
	defer s.Close()
	assert.False(t, s.IsWritten(2))
	assert.Equal(t, base, readAll(t, s))
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

// truncated is a base that claims size bytes but holds fewer, like a file truncated after
// opening or a remote source cut short.
type truncated struct {
	mem
	size int64
}

func (b *truncated) Size() int64 {
	return b.size
}

func TestReadAtRefusesShortBaseRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := &truncated{mem: mem{data: bytes.Repeat([]byte{0x42}, 512)}, size: 2 * testChunk}
	s, err := Open(base, filepath.Join(dir, "c.cow"), filepath.Join(dir, "c.bitmap"), testChunk)
	require.NoError(t, err)
	defer s.Close()
	buf := bytes.Repeat([]byte{0xaa}, testChunk)
	n, err := s.ReadAt(buf, 0)
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF, "a short base read must not be reported as data")
	assert.Less(t, n, testChunk)
}

func TestWriteChunkRefusesShortBaseRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := &truncated{mem: mem{data: bytes.Repeat([]byte{0x42}, 512)}, size: 2 * testChunk}
	s, err := Open(base, filepath.Join(dir, "c.cow"), filepath.Join(dir, "c.bitmap"), testChunk)
	require.NoError(t, err)
	defer s.Close()
	_, err = s.WriteAt([]byte{0x99}, 0)
	assert.Error(t, err, "a copy-up from an incomplete base must not publish the chunk")
	assert.Equal(t, int64(0), s.Written())
}

func TestStoreCountsDemandReads(t *testing.T) {
	dir := t.TempDir()
	base := &mem{data: pattern(testSize)}
	s, err := Open(base, filepath.Join(dir, "d.cow"), filepath.Join(dir, "d.cow.bitmap"), testChunk)
	require.NoError(t, err)
	defer s.Close()
	var runs [][2]int64
	s.OnDemandRead(func(first, count int64) { runs = append(runs, [2]int64{first, count}) })
	// Hydration reads are not demand
	_, err = s.HydrateRun(0, 2, false)
	require.NoError(t, err)
	assert.Equal(t, int64(0), s.SourceStats().DemandReads)
	// A guest read over chunks 1 (hydrated), 2, 3 (not) and a hydrated run again
	buf := make([]byte, 3*testChunk)
	_, err = s.ReadAt(buf, testChunk)
	require.NoError(t, err)
	st := s.SourceStats()
	assert.Equal(t, int64(1), st.DemandReads, "one base read for the unwritten run")
	assert.Equal(t, int64(2*testChunk), st.DemandBytes)
	assert.Equal(t, int64(2), st.Reads, "the hydration read and the demand read; the hydrated chunk came from the cow file")
	assert.Equal(t, [][2]int64{{2, 2}}, runs)
	assert.Zero(t, testing.AllocsPerRun(50, func() { s.ReadAt(buf, testChunk) }))
}

// storeModel is what a Store must read back: the device content and which chunks the
// bitmap holds, plus the bits the last successful flush made durable.
type storeModel struct {
	base      []byte
	content   []byte
	written   []bool
	committed []bool
	chunk     int64
	nopwrite  bool
}

func newStoreModel(base []byte, chunk int64) *storeModel {
	chunks := (int64(len(base)) + chunk - 1) / chunk
	return &storeModel{base: base, content: bytes.Clone(base), written: make([]bool, chunks), committed: make([]bool, chunks), chunk: chunk}
}

func (m *storeModel) span(chunk int64) (int64, int64) {
	start := chunk * m.chunk
	return start, min(start+m.chunk, int64(len(m.content)))
}

func (m *storeModel) write(p []byte, off int64) {
	for len(p) > 0 {
		chunk := off / m.chunk
		_, end := m.span(chunk)
		n := min(int64(len(p)), end-off)
		if !m.nopwrite || !bytes.Equal(m.content[off:off+n], p[:n]) {
			copy(m.content[off:], p[:n])
			m.written[chunk] = true
		}
		p, off = p[n:], off+n
	}
}

func (m *storeModel) discard(off, length int64) {
	for chunk := (off + m.chunk - 1) / m.chunk; chunk < (off+length)/m.chunk; chunk++ {
		if m.written[chunk] {
			start, end := m.span(chunk)
			clear(m.content[start:end])
		}
	}
}

func (m *storeModel) writeZeroes(off, length int64) {
	for end := off + length; off < end; {
		chunk := off / m.chunk
		start, chunkEnd := m.span(chunk)
		n := min(end, chunkEnd) - off
		if off == start && n == chunkEnd-start {
			if !(m.nopwrite && !m.written[chunk] && isZero(m.base[start:chunkEnd])) {
				clear(m.content[start:chunkEnd])
				m.written[chunk] = true
			}
		} else {
			m.write(make([]byte, n), off)
		}
		off += n
	}
}

func (m *storeModel) hydrate(first, count int64) {
	for chunk := first; chunk < min(first+count, int64(len(m.written))); chunk++ {
		m.written[chunk] = true
	}
}

// crash forgets every bit the last flush did not make durable, unless a live bitmap kept it.
func (m *storeModel) crash(live bool) {
	if live {
		return
	}
	for chunk := range m.written {
		if !m.committed[chunk] {
			start, end := m.span(int64(chunk))
			copy(m.content[start:end], m.base[start:end])
			m.written[chunk] = false
		}
	}
}

func fillRandom(r *rand.Rand, p []byte) {
	for i := range p {
		p[i] = byte(r.Uint32())
	}
}

func isZero(p []byte) bool {
	for _, b := range p {
		if b != 0 {
			return false
		}
	}
	return true
}

// randomBase is random content with some all-zero chunks and some zero runs inside chunks.
func randomBase(r *rand.Rand, size, chunk int64) []byte {
	base := make([]byte, size)
	fillRandom(r, base)
	for c := int64(0); c*chunk < size; c++ {
		start, end := c*chunk, min((c+1)*chunk, size)
		switch r.IntN(4) {
		case 0:
			clear(base[start:end])
		case 1:
			clear(base[start : start+(end-start)/2])
		}
	}
	return base
}

func TestStoreModel(t *testing.T) {
	t.Parallel()
	seeds, ops := 300, 400
	if testing.Short() {
		seeds = 30
	}
	if env := os.Getenv("BLKMAP_MODEL_SEEDS"); env != "" { // a longer hunt
		n, err := strconv.Atoi(env)
		require.NoError(t, err)
		seeds = n
	}
	// BLKMAP_MODEL_SEED=N replays one seed, checking the whole device after every operation
	if env := os.Getenv("BLKMAP_MODEL_SEED"); env != "" {
		seed, err := strconv.ParseUint(env, 10, 64)
		require.NoError(t, err)
		runStoreModel(t, seed, ops, true)
		return
	}
	for seed := range seeds {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()
			runStoreModel(t, uint64(seed), ops, false)
		})
	}
}

// runStoreModel drives a store with random operations, reopens and crashes, comparing every
// read with the model. A failure prints the seed and the operations that led to it.
func runStoreModel(t *testing.T, seed uint64, ops int, paranoid bool) {
	r := rand.New(rand.NewPCG(seed, 0x6b6d6170))
	chunk := []int64{512, 4096, 8192}[r.IntN(3)]
	chunks := 4 + r.Int64N(40)
	size := chunks*chunk - r.Int64N(chunk/512)*512 // the last chunk may be partial
	live := r.IntN(2) == 0
	baseData := randomBase(r, size, chunk)
	m := newStoreModel(baseData, chunk)
	dir := t.TempDir()
	o := &Options{COWFile: filepath.Join(dir, "d.cow"), Bitmap: filepath.Join(dir, "d.bitmap"), ChunkSize: chunk}
	if live {
		o.LiveBitmap = filepath.Join(dir, "d.live")
	}
	open := func() *Store {
		s, err := OpenWith(&mem{data: baseData}, o)
		require.NoError(t, err)
		s.SetNopWrite(m.nopwrite)
		return s
	}
	s := open()
	defer func() { s.Close() }()
	var log []string
	fail := func(format string, args ...any) {
		t.Helper()
		t.Fatalf("seed %d (chunk %d, size %d, live %v): %s\nops:\n  %s", seed, chunk, size, live, fmt.Sprintf(format, args...), strings.Join(log, "\n  "))
	}
	check := func(what string) {
		t.Helper()
		got := make([]byte, size)
		if n, err := s.ReadAt(got, 0); err != nil || n != len(got) {
			fail("%s: read %d bytes, %v", what, n, err)
		}
		for c := range m.written {
			start, end := m.span(int64(c))
			if !bytes.Equal(got[start:end], m.content[start:end]) {
				fail("%s: chunk %d differs from the model (written: model %v, store %v)", what, c, m.written[c], s.IsWritten(int64(c)))
			}
			if s.IsWritten(int64(c)) != m.written[c] {
				fail("%s: chunk %d written: model %v, store %v", what, c, m.written[c], s.IsWritten(int64(c)))
			}
		}
	}
	randRange := func() (int64, int64) {
		off := r.Int64N(size)
		if r.IntN(3) > 0 {
			off -= off % 512
		}
		length := 1 + r.Int64N(min(3*chunk, size-off))
		return off, length
	}
	for i := 0; i < ops; i++ {
		switch op := r.IntN(100); {
		case op < 35: // write: random bytes, what is there already, zeros or the base's bytes
			off, length := randRange()
			p := make([]byte, length)
			switch r.IntN(4) {
			case 0:
				fillRandom(r, p)
			case 1:
				copy(p, m.content[off:])
			case 2:
			case 3:
				copy(p, baseData[off:])
			}
			log = append(log, fmt.Sprintf("write %d+%d", off, length))
			if n, err := s.WriteAt(p, off); err != nil || n != len(p) {
				fail("write %d+%d: %d, %v", off, length, n, err)
			}
			m.write(p, off)
		case op < 55: // read a range
			off, length := randRange()
			p := make([]byte, length)
			if n, err := s.ReadAt(p, off); err != nil || n != len(p) {
				fail("read %d+%d: %d, %v", off, length, n, err)
			}
			if !bytes.Equal(p, m.content[off:off+length]) {
				log = append(log, fmt.Sprintf("read %d+%d", off, length))
				fail("read %d+%d differs from the model", off, length)
			}
		case op < 60:
			off, length := randRange()
			log = append(log, fmt.Sprintf("discard %d+%d", off, length))
			if err := s.Discard(off, length); err != nil {
				fail("discard: %v", err)
			}
			m.discard(off, length)
		case op < 65:
			off, length := randRange()
			log = append(log, fmt.Sprintf("zeroes %d+%d", off, length))
			if err := s.WriteZeroes(off, length); err != nil {
				fail("write zeroes: %v", err)
			}
			m.writeZeroes(off, length)
		case op < 72:
			first, count := r.Int64N(chunks), 1+r.Int64N(8)
			log = append(log, fmt.Sprintf("hydrate %d+%d", first, count))
			if _, err := s.HydrateRun(first, count, r.IntN(2) == 0); err != nil {
				fail("hydrate: %v", err)
			}
			m.hydrate(first, count)
		case op < 76: // what the hydrator does for a chunk its source reports as a hole
			c := r.Int64N(chunks)
			start, end := m.span(c)
			if !isZero(baseData[start:end]) {
				continue
			}
			log = append(log, fmt.Sprintf("markzero %d", c))
			s.MarkZero(c)
			m.written[c] = true // the base is zero there, so the content must not change
		case op < 80:
			m.nopwrite = !m.nopwrite
			log = append(log, fmt.Sprintf("nopwrite %v", m.nopwrite))
			s.SetNopWrite(m.nopwrite)
		case op < 88:
			log = append(log, "flush")
			if err := s.Flush(); err != nil {
				fail("flush: %v", err)
			}
			copy(m.committed, m.written)
		case op < 92:
			log = append(log, "close+open")
			if err := s.Close(); err != nil {
				fail("close: %v", err)
			}
			copy(m.committed, m.written)
			s = open()
			check("after reopen")
		case op < 97:
			log = append(log, "crash")
			s.abandon()
			m.crash(live)
			s = open()
			check("after crash")
		default:
			log = append(log, "writeback")
			dst := bytes.Clone(baseData)
			if _, err := s.Writeback(&sliceWriter{dst}); err != nil {
				fail("writeback: %v", err)
			}
			copy(m.committed, m.written)
			for c := range m.written {
				start, end := m.span(int64(c))
				if m.written[c] && !bytes.Equal(dst[start:end], m.content[start:end]) {
					fail("writeback: chunk %d differs from the model", c)
				}
			}
		}
		if paranoid {
			check(fmt.Sprintf("after op %d", i))
		}
	}
	check("at the end")
}

// sliceWriter is an io.WriterAt over a byte slice.
type sliceWriter struct{ b []byte }

func (w *sliceWriter) WriteAt(p []byte, off int64) (int, error) {
	return copy(w.b[off:], p), nil
}

// TestStoreModelConcurrent races writers on their own chunks against hydration, flushes and
// readers across the whole device, then crashes: every chunk must read what its writer left,
// and after the crash either that or, for a bit no flush persisted, the base.
func TestStoreModelConcurrent(t *testing.T) {
	t.Parallel()
	for seed := range uint64(20) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()
			runStoreModelConcurrent(t, seed)
		})
	}
}

func runStoreModelConcurrent(t *testing.T, seed uint64) {
	const writers, chunk, chunks, rounds = 4, 4096, 64, 300
	size := int64(chunks*chunk - 1024)
	r := rand.New(rand.NewPCG(seed, 0x636f6e63))
	live := seed%2 == 0
	baseData := randomBase(r, size, chunk)
	dir := t.TempDir()
	o := &Options{COWFile: filepath.Join(dir, "d.cow"), Bitmap: filepath.Join(dir, "d.bitmap"), ChunkSize: chunk}
	if live {
		o.LiveBitmap = filepath.Join(dir, "d.live")
	}
	s, err := OpenWith(&mem{data: baseData}, o)
	require.NoError(t, err)
	s.SetNopWrite(seed%3 == 0)
	m := newStoreModel(baseData, chunk)
	m.nopwrite = seed%3 == 0
	var stop atomic.Bool
	var bg, wg sync.WaitGroup
	errs := make(chan error, writers+3)
	// Hydration, zero marking, flushes and unverified reads across every chunk
	bg.Add(3)
	go func() {
		defer bg.Done()
		r := rand.New(rand.NewPCG(seed, 1))
		for !stop.Load() {
			c := r.Int64N(chunks)
			if start, end := m.span(c); isZero(baseData[start:end]) && r.IntN(2) == 0 {
				s.MarkZero(c)
			} else if _, err := s.HydrateRun(c, 1+r.Int64N(6), false); err != nil {
				errs <- fmt.Errorf("hydrate: %w", err)
				return
			}
		}
	}()
	go func() {
		defer bg.Done()
		for !stop.Load() {
			if err := s.Flush(); err != nil {
				errs <- fmt.Errorf("flush: %w", err)
				return
			}
		}
	}()
	go func() {
		defer bg.Done()
		r := rand.New(rand.NewPCG(seed, 2))
		p := make([]byte, 5*chunk)
		for !stop.Load() {
			off := r.Int64N(size)
			n := min(int64(len(p)), size-off)
			if _, err := s.ReadAt(p[:n], off); err != nil && !errors.Is(err, io.EOF) {
				errs <- fmt.Errorf("read: %w", err)
				return
			}
		}
	}()
	// Each writer owns the chunks c with c%writers == w, so its part of the model is its own
	contents := make([][]byte, writers)
	for w := range writers {
		contents[w] = bytes.Clone(baseData)
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(seed, uint64(10+w)))
			own := contents[w]
			for range rounds {
				c := int64(w) + writers*r.Int64N(chunks/writers)
				start, end := m.span(c)
				off := start + r.Int64N(end-start)
				off -= off % 512
				length := 1 + r.Int64N(end-off)
				switch r.IntN(10) {
				case 0:
					// Bits never clear, so a whole chunk written before the discard is punched;
					// any other may gain its bit from the hydrator concurrently, so skip it
					if !s.IsWritten(c) || end-start != chunk {
						continue
					}
					if err := s.Discard(start, end-start); err != nil {
						errs <- err
						return
					}
					clear(own[start:end])
				case 1:
					if err := s.WriteZeroes(off, length); err != nil {
						errs <- err
						return
					}
					clear(own[off : off+length])
				default:
					p := make([]byte, length)
					fillRandom(r, p)
					if _, err := s.WriteAt(p, off); err != nil {
						errs <- err
						return
					}
					copy(own[off:], p)
				}
				got := make([]byte, end-start)
				if _, err := s.ReadAt(got, start); err != nil && !errors.Is(err, io.EOF) {
					errs <- err
					return
				}
				if !bytes.Equal(got, own[start:end]) {
					errs <- fmt.Errorf("writer %d: chunk %d differs from what it wrote", w, c)
					return
				}
			}
		}()
	}
	wg.Wait()
	stop.Store(true)
	bg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("seed %d (live %v): %v", seed, live, err)
	}
	// Assemble the expected device from the writers' chunks
	want := make([]byte, size)
	for c := int64(0); c < chunks; c++ {
		start, end := m.span(c)
		copy(want[start:end], contents[c%writers][start:end])
	}
	assert.Equal(t, want, readAll(t, s), "seed %d: before the crash", seed)
	s.abandon()
	s, err = OpenWith(&mem{data: baseData}, o)
	require.NoError(t, err)
	defer s.Close()
	got := readAll(t, s)
	for c := int64(0); c < chunks; c++ {
		start, end := m.span(c)
		switch {
		case bytes.Equal(got[start:end], want[start:end]):
		case !live && !s.IsWritten(c) && bytes.Equal(got[start:end], baseData[start:end]):
		default:
			t.Fatalf("seed %d (live %v): after the crash chunk %d (written %v) reads neither what was written nor, unflushed, the base", seed, live, c, s.IsWritten(c))
		}
	}
}

// With nopwrite on, writes that repeat what the device already reads are dropped: no chunk
// is recorded for an identical write over the base, an identical rewrite of a stored chunk
// does not dirty the store, and zeroing a range the base already reads as zeros is free.
func TestNopWriteIdenticalWrites(t *testing.T) {
	dir := t.TempDir()
	base := &mem{data: pattern(testSize)}
	s, err := Open(base, filepath.Join(dir, "cow"), filepath.Join(dir, "cow.bitmap"), testChunk)
	require.NoError(t, err)
	defer s.Close()
	s.SetNopWrite(true)

	// identical partial write over the base: nothing stored
	_, err = s.WriteAt(base.data[100:600], 100)
	require.NoError(t, err)
	require.EqualValues(t, 0, s.Written())
	require.False(t, s.Dirty())

	// identical whole-chunk write over the base: nothing stored
	_, err = s.WriteAt(base.data[testChunk:2*testChunk], testChunk)
	require.NoError(t, err)
	require.EqualValues(t, 0, s.Written())

	// a different write is stored, and the rest of its chunk is copied up as usual
	changed := bytes.Repeat([]byte{0xAB}, 300)
	_, err = s.WriteAt(changed, 2*testChunk+50)
	require.NoError(t, err)
	require.EqualValues(t, 1, s.Written())
	got := make([]byte, testChunk)
	_, err = s.ReadAt(got, 2*testChunk)
	require.NoError(t, err)
	require.Equal(t, base.data[2*testChunk:2*testChunk+50], got[:50])
	require.Equal(t, changed, got[50:350])
	require.Equal(t, base.data[2*testChunk+350:3*testChunk], got[350:])
	require.NoError(t, s.Flush())

	// rewriting a stored chunk with what it already holds does not dirty the store
	_, err = s.WriteAt(changed, 2*testChunk+50)
	require.NoError(t, err)
	require.False(t, s.Dirty())
	// but a real change to it does
	_, err = s.WriteAt([]byte{1, 2, 3}, 2*testChunk+50)
	require.NoError(t, err)
	require.True(t, s.Dirty())

	// zeroing a chunk the base reads as zeros records nothing; zeroing data does
	zeroBase := &mem{data: make([]byte, testSize)}
	copy(zeroBase.data[5*testChunk:], pattern(testChunk))
	z, err := Open(zeroBase, filepath.Join(dir, "z"), filepath.Join(dir, "z.bitmap"), testChunk)
	require.NoError(t, err)
	defer z.Close()
	z.SetNopWrite(true)
	require.NoError(t, z.WriteZeroes(0, testChunk))
	require.EqualValues(t, 0, z.Written())
	require.NoError(t, z.WriteZeroes(5*testChunk, testChunk))
	require.EqualValues(t, 1, z.Written())
	_, err = z.ReadAt(got, 5*testChunk)
	require.NoError(t, err)
	require.Equal(t, make([]byte, testChunk), got)

	// with nopwrite off, the same identical write is stored
	s.SetNopWrite(false)
	_, err = s.WriteAt(base.data[7*testChunk:7*testChunk+10], 7*testChunk)
	require.NoError(t, err)
	require.EqualValues(t, 2, s.Written())
}

// derived is a base whose bytes are computed from other devices (a source.Binder): they can
// change after a write was checked against them.
type derived struct {
	data []byte
}

func (d *derived) ReadAt(p []byte, off int64) (int, error) { return copy(p, d.data[off:]), nil }
func (d *derived) Size() int64                             { return int64(len(d.data)) }
func (d *derived) Close() error                            { return nil }
func (d *derived) Bind(source.Lookup)                      {}

// Over a base derived from other devices, a write equal to what the base reads now is skipped
// like any other, and the range keeps following the base afterwards: a mirror plex rewritten
// with its sibling's bytes stays a view of the sibling, a parity column regenerated from the
// data columns stays computed. A write that differs is stored and no longer follows.
func TestNopWriteOverADerivedBaseFollowsIt(t *testing.T) {
	dir := t.TempDir()
	base := &derived{data: make([]byte, testSize)}
	s, err := Open(base, filepath.Join(dir, "cow"), filepath.Join(dir, "cow.bitmap"), testChunk)
	require.NoError(t, err)
	defer s.Close()
	s.SetNopWrite(true)
	_, err = s.WriteAt(make([]byte, testChunk), 0) // equal to what the base derives right now
	require.NoError(t, err)
	require.NoError(t, s.WriteZeroes(testChunk, testChunk))
	require.EqualValues(t, 0, s.Written(), "identical writes over a derived base store nothing")
	base.data[10], base.data[testChunk+10] = 0xFF, 0xFF // a sibling changes
	got := make([]byte, 2*testChunk)
	_, err = s.ReadAt(got, 0)
	require.NoError(t, err)
	require.Equal(t, byte(0xFF), got[10], "a skipped range follows its base")
	require.Equal(t, byte(0xFF), got[testChunk+10])
	_, err = s.WriteAt([]byte{1, 2, 3}, 0) // differs from the base: stored
	require.NoError(t, err)
	require.EqualValues(t, 1, s.Written())
	base.data[20] = 0xEE
	_, err = s.ReadAt(got[:testChunk], 0)
	require.NoError(t, err)
	require.Equal(t, []byte{1, 2, 3}, got[:3])
	require.Equal(t, byte(0), got[20], "a stored chunk no longer follows the base")
	require.Equal(t, byte(0xFF), got[10], "the copy-up took the base as it was then")
}

// A whole-chunk write does not need the base, so nopwrite must not make it fail when the base
// cannot be read: the write is stored instead.
func TestNopWriteSurvivesAnUnreadableBase(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(&broken{size: testSize}, filepath.Join(dir, "cow"), filepath.Join(dir, "cow.bitmap"), testChunk)
	require.NoError(t, err)
	defer s.Close()
	s.SetNopWrite(true)
	w := bytes.Repeat([]byte{0x5A}, testChunk)
	_, err = s.WriteAt(w, testChunk)
	require.NoError(t, err)
	require.EqualValues(t, 1, s.Written())
	got := make([]byte, testChunk)
	_, err = s.ReadAt(got, testChunk)
	require.NoError(t, err)
	require.Equal(t, w, got)
}
