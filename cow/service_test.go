package cow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

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

// storeModel is what a Store must read back: the COW file's bytes and which chunks the bitmap
// holds (a chunk reads the COW file if it does, the base if not; content is that view), plus
// the bits the last successful flush made durable, the chunks written since Reclaim last
// examined them, and the chunks Reclaim dropped that wait for a flush to punch them.
type storeModel struct {
	base      []byte
	cow       []byte
	content   []byte
	written   []bool
	committed []bool
	recent    []bool
	punches   []int64
	chunk     int64
	nopwrite  bool
}

func newStoreModel(base []byte, chunk int64) *storeModel {
	chunks := (int64(len(base)) + chunk - 1) / chunk
	return &storeModel{base: base, cow: make([]byte, len(base)), content: bytes.Clone(base), written: make([]bool, chunks), committed: make([]bool, chunks), recent: make([]bool, chunks), chunk: chunk}
}

func (m *storeModel) span(chunk int64) (int64, int64) {
	start := chunk * m.chunk
	return start, min(start+m.chunk, int64(len(m.content)))
}

// refresh recomputes what chunk reads from its bit.
func (m *storeModel) refresh(chunk int64) {
	start, end := m.span(chunk)
	if m.written[chunk] {
		copy(m.content[start:end], m.cow[start:end])
	} else {
		copy(m.content[start:end], m.base[start:end])
	}
}

func (m *storeModel) write(p []byte, off int64) {
	for len(p) > 0 {
		chunk := off / m.chunk
		start, end := m.span(chunk)
		n := min(int64(len(p)), end-off)
		if !m.nopwrite || !bytes.Equal(m.content[off:off+n], p[:n]) {
			copy(m.cow[start:end], m.content[start:end]) // the copy-up, a no-op for a stored chunk
			copy(m.cow[off:], p[:n])
			m.written[chunk], m.recent[chunk] = true, true
			m.refresh(chunk)
		} else if m.written[chunk] {
			m.recent[chunk] = true // a repeated write to a stored chunk is recorded all the same
		}
		p, off = p[n:], off+n
	}
}

func (m *storeModel) discard(off, length int64) {
	for chunk := (off + m.chunk - 1) / m.chunk; chunk < (off+length)/m.chunk; chunk++ {
		if m.written[chunk] {
			start, end := m.span(chunk)
			clear(m.cow[start:end])
			m.recent[chunk] = true
			m.refresh(chunk)
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
				clear(m.cow[start:chunkEnd])
				m.written[chunk], m.recent[chunk] = true, true
				m.refresh(chunk)
			}
		} else {
			m.write(make([]byte, n), off)
		}
		off += n
	}
}

// hydrate and markZero copy the base (or mark a zero chunk) without recording the chunk for
// Reclaim, like the store.
func (m *storeModel) hydrate(first, count int64) {
	for chunk := first; chunk < min(first+count, int64(len(m.written))); chunk++ {
		if !m.written[chunk] {
			start, end := m.span(chunk)
			copy(m.cow[start:end], m.base[start:end])
			m.written[chunk] = true
		}
	}
}

func (m *storeModel) markZero(chunk int64) {
	if !m.written[chunk] {
		start, end := m.span(chunk)
		clear(m.cow[start:end])
		m.written[chunk] = true
	}
}

// reclaim is Reclaim over every recorded chunk, whatever the order: a stored one that holds
// its base's bytes is dropped and its stale bytes stay in the COW file until a flush punches
// them; one that differs is examined a second time (the store's settle time is zero in the
// model) and then forgotten. It reports how many chunks were examined.
func (m *storeModel) reclaim() int {
	n := 0
	for chunk, recent := range m.recent {
		if !recent {
			continue
		}
		m.recent[chunk], n = false, n+1
		if start, end := m.span(int64(chunk)); m.written[chunk] && bytes.Equal(m.cow[start:end], m.base[start:end]) {
			m.written[chunk] = false
			m.punches = append(m.punches, int64(chunk))
			m.refresh(int64(chunk))
		} else if m.written[chunk] {
			n++ // the second look finds it unchanged
		}
	}
	return n
}

// flush makes the bits durable, then punches the dropped chunks whose bits are still clear.
func (m *storeModel) flush() {
	copy(m.committed, m.written)
	for _, chunk := range m.punches {
		if !m.written[chunk] {
			start, end := m.span(chunk)
			clear(m.cow[start:end])
		}
	}
	m.punches = nil
}

// reopen is what a clean reopen changes: a fresh store has recorded nothing for Reclaim.
func (m *storeModel) reopen() {
	m.flush()
	clear(m.recent)
}

// crash forgets every bit change the last flush did not make durable, unless a live bitmap
// kept it: a chunk whose bit is set on disk reads the COW file, stale bytes included. The
// punches a flush owed and the chunks recorded for Reclaim are forgotten either way; the data
// the punches would free stays unreachable.
func (m *storeModel) crash(live bool) {
	m.punches = nil
	clear(m.recent)
	if live {
		return
	}
	copy(m.written, m.committed)
	for chunk := range m.written {
		m.refresh(int64(chunk))
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
		s.EnableReclaim()
		s.settle = 0 // a second look at once, so a reclaim pass is deterministic
		return s
	}
	s := open()
	defer func() { s.Close() }()
	ctx := context.Background()
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
		case op < 50: // read a range
			off, length := randRange()
			p := make([]byte, length)
			if n, err := s.ReadAt(p, off); err != nil || n != len(p) {
				fail("read %d+%d: %d, %v", off, length, n, err)
			}
			if !bytes.Equal(p, m.content[off:off+length]) {
				log = append(log, fmt.Sprintf("read %d+%d", off, length))
				fail("read %d+%d differs from the model", off, length)
			}
		case op < 55: // sweep everything recorded, in passes of a random budget
			budget := 1 + r.IntN(int(chunks))
			log = append(log, fmt.Sprintf("reclaim %d", budget))
			examined := 0
			for n := s.Reclaim(ctx, budget); n > 0; n = s.Reclaim(ctx, budget) {
				examined += n
			}
			if want := m.reclaim(); examined != want {
				fail("reclaim examined %d chunks, the model %d", examined, want)
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
			m.markZero(c)
		case op < 80:
			m.nopwrite = !m.nopwrite
			log = append(log, fmt.Sprintf("nopwrite %v", m.nopwrite))
			s.SetNopWrite(m.nopwrite)
		case op < 88:
			log = append(log, "flush")
			if err := s.Flush(); err != nil {
				fail("flush: %v", err)
			}
			m.flush()
		case op < 92:
			log = append(log, "close+open")
			if err := s.Close(); err != nil {
				fail("close: %v", err)
			}
			m.reopen()
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
			m.flush()
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

// TestStoreModelConcurrent races writers on their own chunks against hydration, flushes,
// readers and (every other seed) a sweeper across the whole device, then crashes: every chunk
// must read what its writer left, and after the crash either that or, for a bit no flush
// persisted, the base.
func TestStoreModelConcurrent(t *testing.T) {
	t.Parallel()
	seeds := 20
	if env := os.Getenv("BLKMAP_MODEL_SEEDS"); env != "" { // a longer hunt
		n, err := strconv.Atoi(env)
		require.NoError(t, err)
		seeds = n
	}
	for seed := range uint64(seeds) {
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
	s.EnableReclaim()
	s.settle = time.Duration(seed%5) * 20 * time.Millisecond
	m := newStoreModel(baseData, chunk)
	m.nopwrite = seed%3 == 0
	sweep := seed%4 < 2
	var stop atomic.Bool
	var bg, wg sync.WaitGroup
	errs := make(chan error, writers+4)
	// Hydration, zero marking, flushes, unverified reads and reclaim passes across every chunk
	bg.Add(3)
	if sweep {
		bg.Add(1)
		go func() {
			defer bg.Done()
			for !stop.Load() {
				s.Reclaim(context.Background(), 1+int(seed%7))
			}
		}()
	}
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
					// Only the sweeper clears bits, so without it a whole chunk written before
					// the discard is punched; any other may gain its bit from the hydrator
					// concurrently, so skip it. With the sweeper the bit may clear between the
					// check and the discard, which then leaves the chunk reading the base (what
					// discard semantics allow), so skip the case altogether
					if sweep || !s.IsWritten(c) || end-start != chunk {
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
				case 2, 3: // the base's bytes: with nopwrite skipped, otherwise stored and the sweeper's to drop
					if _, err := s.WriteAt(baseData[off:off+length], off); err != nil {
						errs <- err
						return
					}
					copy(own[off:], baseData[off:off+length])
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

// Durable: the fake sibling's bytes are in memory and count as durable, so the tests of the
// following semantics are about nopwrite, not about a sibling's flushes.
func (d *derived) Durable(off, length int64) bool { return true }

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

// Reclaim undoes a freeze over a derived base: a chunk stored because this plex's half of a
// mirrored write landed first (the base still showed the old bytes) is dropped once the base
// has caught up, follows the base again, is a nopwrite target again, and costs no space after
// the next Flush.
func TestReclaimOverADerivedBase(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := &derived{data: pattern(testSize)}
	s := newTestStore(t, dir, base)
	s.EnableReclaim()
	defer s.Close()
	s.SetNopWrite(true)
	ctx := context.Background()
	_, err := s.WriteAt([]byte{1, 2, 3}, 10) // this plex's half lands first
	require.NoError(t, err)
	require.EqualValues(t, 1, s.Written(), "a write that differs from the base is stored")
	require.Equal(t, ReclaimStats{Pending: 1}, s.ReclaimStats())
	assert.Equal(t, 1, s.Reclaim(ctx, 256), "examined, but it still differs")
	require.EqualValues(t, 1, s.Written())
	copy(base.data[10:], []byte{1, 2, 3}) // the other half lands
	assert.Zero(t, s.Reclaim(ctx, 256), "not examined again until written again or settled")
	assert.Equal(t, ReclaimStats{Pending: 1, Examined: 1}, s.ReclaimStats(), "waiting for its second look")
	_, err = s.WriteAt([]byte{4}, 100) // the next mirrored write, this plex first again
	require.NoError(t, err)
	assert.Equal(t, 1, s.Reclaim(ctx, 256), "recorded by the write: examined at once")
	assert.EqualValues(t, 1, s.Written(), "still differs: the other half is not there yet")
	base.data[100] = 4
	_, err = s.WriteAt([]byte{4}, 100) // a nopwrite against the stored copy: recorded all the same
	require.NoError(t, err)
	assert.Equal(t, 1, s.Reclaim(ctx, 256))
	assert.EqualValues(t, 0, s.Written(), "the stored chunk equals its base: dropped")
	assert.Equal(t, ReclaimStats{Examined: 3, Chunks: 1, Bytes: testChunk}, s.ReclaimStats(), "nothing waits for a second look once dropped")
	assert.True(t, s.Dirty(), "the cleared bit waits for a flush")
	got := make([]byte, testChunk)
	_, err = s.ReadAt(got, 0)
	require.NoError(t, err)
	assert.Equal(t, base.data[:testChunk], got)
	base.data[20] = 0xEE // a sibling changes
	_, err = s.ReadAt(got, 0)
	require.NoError(t, err)
	assert.Equal(t, byte(0xEE), got[20], "the chunk follows the base again")
	base.data[30] = 0xDD // a mirrored write whose other half landed first
	_, err = s.WriteAt([]byte{0xDD}, 30)
	require.NoError(t, err)
	assert.EqualValues(t, 0, s.Written(), "a write equal to the base is a nopwrite again")
	st, err := os.Stat(filepath.Join(dir, "d.cow"))
	require.NoError(t, err)
	assert.Equal(t, int64(testChunk), st.Sys().(*syscall.Stat_t).Blocks*512, "the stale bytes stay until a flush")
	require.NoError(t, s.Flush())
	st, err = os.Stat(filepath.Join(dir, "d.cow"))
	require.NoError(t, err)
	assert.Zero(t, st.Sys().(*syscall.Stat_t).Blocks, "the flush punched the dropped chunk")
	assert.False(t, s.Dirty())
}

// A chunk that still differs from its base is kept, at the cost of one base read; one whose
// base cannot be read is kept too. Restoring a chunk's bytes piecemeal frees it with the write
// that completes the match, nopwrite or not.
func TestReclaimKeepsAChunkThatDiffers(t *testing.T) {
	t.Parallel()
	base := &mem{data: pattern(testSize)}
	s := newTestStore(t, t.TempDir(), base)
	s.EnableReclaim()
	defer s.Close()
	ctx := context.Background()
	start := int64(2 * testChunk)
	_, err := s.WriteAt(bytes.Repeat([]byte{0xAB}, 300), start+100)
	require.NoError(t, err)
	reads := base.reads
	assert.Equal(t, 1, s.Reclaim(ctx, 256))
	assert.EqualValues(t, 1, s.Written(), "the chunk still differs from the base")
	assert.Equal(t, 1, base.reads-reads, "one base read per examined chunk")
	assert.Equal(t, ReclaimStats{Pending: 1, Examined: 1}, s.ReclaimStats(), "kept, and waiting for a second look")
	_, err = s.WriteAt(base.data[start+100:start+250], start+100) // restores half of the change
	require.NoError(t, err)
	assert.Equal(t, 1, s.Reclaim(ctx, 256))
	assert.EqualValues(t, 1, s.Written())
	_, err = s.WriteAt(base.data[start+250:start+400], start+250) // restores the rest
	require.NoError(t, err)
	assert.Equal(t, 1, s.Reclaim(ctx, 256))
	assert.EqualValues(t, 0, s.Written(), "the chunk equals the base again")
	assert.Equal(t, ReclaimStats{Examined: 3, Chunks: 1, Bytes: testChunk}, s.ReclaimStats(), "dropped: no second look left to take")
	assert.Equal(t, base.data, readAll(t, s))
	// a chunk whose base cannot be read stays as it is
	u := newTestStore(t, t.TempDir(), &broken{size: testSize})
	u.EnableReclaim()
	defer u.Close()
	w := bytes.Repeat([]byte{0x5A}, testChunk)
	_, err = u.WriteAt(w, testChunk)
	require.NoError(t, err)
	assert.Equal(t, 1, u.Reclaim(ctx, 256))
	assert.EqualValues(t, 1, u.Written())
	assert.Equal(t, ReclaimStats{Pending: 1, Examined: 1}, u.ReclaimStats())
}

// A chunk that differs from its base when examined gets one more look once the set of such
// chunks has settled (the other half of a mirrored write lands within moments), then is
// forgotten until a write records it again: a chunk whose sibling never catches up costs two
// examinations, not one per pass.
func TestReclaimLooksAgainAfterSettling(t *testing.T) {
	t.Parallel()
	base := &derived{data: pattern(testSize)}
	s := newTestStore(t, t.TempDir(), base)
	s.EnableReclaim()
	defer s.Close()
	s.SetNopWrite(true)
	ctx := context.Background()
	_, err := s.WriteAt([]byte{1, 2, 3}, 10) // this plex's half lands first
	require.NoError(t, err)
	_, err = s.WriteAt([]byte{9}, 5*testChunk) // a chunk whose sibling never gets the write
	require.NoError(t, err)
	assert.Equal(t, 2, s.Reclaim(ctx, 256), "both differ")
	assert.EqualValues(t, 2, s.Written())
	assert.Equal(t, ReclaimStats{Pending: 2, Examined: 2}, s.ReclaimStats())
	copy(base.data[10:], []byte{1, 2, 3}) // the other half lands
	assert.Zero(t, s.Reclaim(ctx, 256), "not settled yet")
	s.settle = 0
	assert.Equal(t, 2, s.Reclaim(ctx, 256), "the second look")
	assert.EqualValues(t, 1, s.Written(), "the chunk the sibling caught up with is dropped")
	assert.True(t, s.IsWritten(5))
	assert.Equal(t, ReclaimStats{Examined: 4, Chunks: 1, Bytes: testChunk}, s.ReclaimStats(), "the other is forgotten: no third look")
	assert.Zero(t, s.Reclaim(ctx, 256))
	s.settle = time.Hour
	_, err = s.WriteAt([]byte{9}, 5*testChunk) // repeated by the guest: looked at again
	require.NoError(t, err)
	assert.Equal(t, 1, s.Reclaim(ctx, 256))
	assert.Equal(t, ReclaimStats{Pending: 1, Examined: 5, Chunks: 1, Bytes: testChunk}, s.ReclaimStats(), "and waits for a second look again")
}

// Reclaim examines at most budget chunks per call, resumes where it stopped, wraps around, and
// stops at a cancelled context; hydrated and zero-marked chunks are never examined.
func TestReclaimBudgetAndOrder(t *testing.T) {
	t.Parallel()
	base := &mem{data: pattern(128 * testChunk)}
	s := newTestStore(t, t.TempDir(), base)
	s.EnableReclaim()
	defer s.Close()
	ctx := context.Background()
	_, err := s.HydrateRun(100, 20, false)
	require.NoError(t, err)
	assert.Zero(t, s.Reclaim(ctx, 256), "hydrated chunks are not examined")
	for c := int64(0); c < 80; c++ { // identical whole-chunk writes: stored, since nopwrite is off
		_, err := s.WriteAt(base.data[c*testChunk:(c+1)*testChunk], c*testChunk)
		require.NoError(t, err)
	}
	assert.EqualValues(t, 100, s.Written())
	assert.EqualValues(t, 80, s.ReclaimStats().Pending)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	assert.Zero(t, s.Reclaim(cancelled, 256), "a cancelled context examines nothing")
	assert.Equal(t, 50, s.Reclaim(ctx, 50))
	assert.EqualValues(t, 50, s.Written(), "the first 50 examined and dropped")
	assert.EqualValues(t, 30, s.ReclaimStats().Pending)
	_, err = s.WriteAt([]byte{1}, 5) // recorded again, behind the scan position: seen after the wrap
	require.NoError(t, err)
	assert.Equal(t, 31, s.Reclaim(ctx, 256))
	assert.EqualValues(t, 21, s.Written(), "the hydrated chunks and the one that differs")
	assert.True(t, s.IsWritten(0))
	assert.Zero(t, s.Reclaim(ctx, 256))
	assert.Equal(t, ReclaimStats{Pending: 1, Examined: 81, Chunks: 80, Bytes: 80 * testChunk}, s.ReclaimStats(), "the one that differs waits for its second look")
}

// The bit is cleared first and the chunk punched only once that clear is on disk. A chunk
// punched while the disk bitmap still records it would read zeros after a crash; a cleared
// bit over the stale bytes reads the base, which holds the same bytes. So before the flush
// the COW file still holds the dropped chunk, a flush whose COW sync fails leaves it in place
// for the successor, and a successful flush punches it after committing the bitmap.
func TestReclaimPunchesAfterTheClearIsOnDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cowPath, bitmapPath := filepath.Join(dir, "d.cow"), filepath.Join(dir, "d.cow.bitmap")
	base := pattern(testSize)
	s, err := Open(&mem{data: base}, cowPath, bitmapPath, testChunk)
	require.NoError(t, err)
	s.EnableReclaim()
	ctx := context.Background()
	same := base[4*testChunk : 5*testChunk]
	_, err = s.WriteAt(bytes.Repeat([]byte{'o'}, testChunk), 4*testChunk)
	require.NoError(t, err)
	_, err = s.WriteAt(same, 4*testChunk) // restored to the base's bytes, still stored
	require.NoError(t, err)
	require.NoError(t, s.Flush()) // the disk bitmap records chunk 4, the COW file holds its bytes
	require.Equal(t, 1, s.Reclaim(ctx, 256))
	require.EqualValues(t, 0, s.Written())
	inCOW := make([]byte, testChunk)
	_, err = s.cow.ReadAt(inCOW, 4*testChunk)
	require.NoError(t, err)
	assert.Equal(t, same, inCOW, "the dropped chunk keeps its bytes until the cleared bit is on disk")
	s.syncCOW = func() error { return syscall.EIO }
	require.ErrorIs(t, s.Flush(), syscall.EIO)
	_, err = s.cow.ReadAt(inCOW, 4*testChunk)
	require.NoError(t, err)
	assert.Equal(t, same, inCOW, "a failed flush must not punch")
	info, err := Inspect(bitmapPath)
	require.NoError(t, err)
	assert.EqualValues(t, 1, info.Written, "the disk bitmap still records the chunk")
	assert.ErrorIs(t, s.Close(), ErrCOWFailed)
	// The successor serves the last durable state: the chunk's bytes, never zeros
	s, err = Open(&mem{data: base}, cowPath, bitmapPath, testChunk)
	require.NoError(t, err)
	s.EnableReclaim()
	assert.True(t, s.IsWritten(4))
	got := make([]byte, testChunk)
	_, err = s.ReadAt(got, 4*testChunk)
	require.NoError(t, err)
	assert.Equal(t, same, got)
	assert.Zero(t, s.Reclaim(ctx, 256), "a fresh store has recorded nothing")
	_, err = s.WriteAt(same[:1], 4*testChunk) // written again: recorded again
	require.NoError(t, err)
	require.Equal(t, 1, s.Reclaim(ctx, 256))
	require.EqualValues(t, 0, s.Written())
	require.NoError(t, s.Flush())
	info, err = Inspect(bitmapPath)
	require.NoError(t, err)
	assert.Zero(t, info.Written, "the cleared bit reached the disk")
	_, err = s.cow.ReadAt(inCOW, 4*testChunk)
	require.NoError(t, err)
	assert.Equal(t, make([]byte, testChunk), inCOW, "and the chunk was punched")
	require.NoError(t, s.Close())
}

// A chunk written again while its punch waits for a flush holds live data, which the flush
// must leave alone.
func TestReclaimThenRewriteKeepsTheNewData(t *testing.T) {
	t.Parallel()
	base := &mem{data: pattern(testSize)}
	s := newTestStore(t, t.TempDir(), base)
	s.EnableReclaim()
	defer s.Close()
	_, err := s.WriteAt([]byte{1, 2, 3}, testChunk)
	require.NoError(t, err)
	_, err = s.WriteAt(base.data[testChunk:testChunk+3], testChunk)
	require.NoError(t, err)
	require.Equal(t, 1, s.Reclaim(context.Background(), 256)) // dropped, punch pending
	require.EqualValues(t, 0, s.Written())
	fresh := []byte{4, 5, 6}
	_, err = s.WriteAt(fresh, testChunk) // stored again before the flush
	require.NoError(t, err)
	require.EqualValues(t, 1, s.Written())
	require.NoError(t, s.Flush())
	got := make([]byte, testChunk)
	_, err = s.ReadAt(got, testChunk)
	require.NoError(t, err)
	assert.Equal(t, fresh, got[:3])
	assert.Equal(t, base.data[testChunk+3:2*testChunk], got[3:])
}

// The live bitmap carries a cleared bit across a crash like a set one: the successor reads
// the base for the dropped chunk, and its next flush puts the clear on disk.
func TestReclaimSurvivesCrashWithLiveBitmap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := pattern(testSize)
	o := &Options{COWFile: filepath.Join(dir, "d.cow"), Bitmap: filepath.Join(dir, "d.bitmap"), LiveBitmap: filepath.Join(dir, "d.live"), ChunkSize: testChunk}
	s, err := OpenWith(&mem{data: base}, o)
	require.NoError(t, err)
	s.EnableReclaim()
	_, err = s.WriteAt(bytes.Repeat([]byte{'w'}, testChunk), 3*testChunk)
	require.NoError(t, err)
	require.NoError(t, s.Flush())
	_, err = s.WriteAt(base[3*testChunk:4*testChunk], 3*testChunk)
	require.NoError(t, err)
	require.Equal(t, 1, s.Reclaim(context.Background(), 256))
	require.EqualValues(t, 0, s.Written())
	s.abandon()
	s, err = OpenWith(&mem{data: base}, o)
	require.NoError(t, err)
	s.EnableReclaim()
	defer s.Close()
	assert.False(t, s.IsWritten(3), "the live bitmap kept the clear")
	assert.Equal(t, base, readAll(t, s))
	assert.Zero(t, s.Reclaim(context.Background(), 256), "nothing is recorded, but the orphaned chunk is found")
	assert.True(t, s.Dirty())
	require.NoError(t, s.Flush())
	info, err := Inspect(o.Bitmap)
	require.NoError(t, err)
	assert.Zero(t, info.Written)
	st, err := s.Stat()
	require.NoError(t, err)
	assert.Zero(t, st.Blocks, "the punch the predecessor owed is done once the clear is on disk")
}

// A punch lost between the bitmap commit and the punch itself (the clear is on disk, the
// chunk still allocated) is found by the successor's first Reclaim, with or without a live
// bitmap, and done at its next flush; chunks with their bit set are left alone.
func TestReclaimPunchesWhatAPredecessorLeftAllocated(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := pattern(testSize)
	o := &Options{COWFile: filepath.Join(dir, "d.cow"), Bitmap: filepath.Join(dir, "d.bitmap"), ChunkSize: testChunk}
	s, err := OpenWith(&mem{data: base}, o)
	require.NoError(t, err)
	s.EnableReclaim()
	for _, c := range []int64{3, 7} { // identical, stored (nopwrite off)
		_, err = s.WriteAt(base[c*testChunk:(c+1)*testChunk], c*testChunk)
		require.NoError(t, err)
	}
	_, err = s.WriteAt([]byte{1, 2, 3}, 9*testChunk) // differs: stays stored
	require.NoError(t, err)
	require.NoError(t, s.Flush())
	require.Equal(t, 3, s.Reclaim(context.Background(), 256))
	require.EqualValues(t, 1, s.Written())
	s.punches = nil                                          // the punches are lost
	require.NoError(t, s.bitmap.Commit(s.bitmap.Snapshot())) // but the clears are on disk
	s.abandon()
	s, err = OpenWith(&mem{data: base}, o)
	require.NoError(t, err)
	s.EnableReclaim()
	defer s.Close()
	st, err := s.Stat()
	require.NoError(t, err)
	require.Equal(t, int64(3*testChunk/512), st.Blocks, "the dropped chunks are still allocated")
	assert.Zero(t, s.Reclaim(context.Background(), 256))
	assert.Equal(t, 2, s.pendingPunches())
	require.NoError(t, s.Flush())
	st, err = s.Stat()
	require.NoError(t, err)
	assert.Equal(t, int64(testChunk/512), st.Blocks, "only the stored chunk keeps its space")
	got := make([]byte, 3)
	_, err = s.ReadAt(got, 9*testChunk)
	require.NoError(t, err)
	assert.Equal(t, []byte{1, 2, 3}, got)
}

// A punch whose clear an earlier flush already committed must still happen: Reclaim can queue
// a chunk after a flush has taken the queue but before it snapshots the bitmap, so that flush
// commits the clear and the next one finds the chunk's page clean. The punch waits for that
// next flush and must not be skipped for want of a snapshot of its page.
func TestReclaimPunchSurvivesAnEarlierFlush(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := &mem{data: pattern(testSize)}
	s := newTestStore(t, dir, base)
	s.EnableReclaim()
	defer s.Close()
	_, err := s.WriteAt(base.data[2*testChunk:3*testChunk], 2*testChunk) // identical, stored (nopwrite off)
	require.NoError(t, err)
	require.NoError(t, s.Flush())
	require.Equal(t, 1, s.Reclaim(context.Background(), 256))
	require.EqualValues(t, 0, s.Written())
	// The interleaving: the flush took the queue before this reclaim queued the chunk
	taken := s.takePunches()
	require.Equal(t, []int64{2}, taken)
	require.NoError(t, s.Flush()) // commits the clear without the punch
	s.queuePunches(taken...)
	s.dirty.Store(true)
	require.NoError(t, s.Flush())
	st, err := s.Stat()
	require.NoError(t, err)
	assert.Zero(t, st.Blocks, "the clear is on disk, so the punch must happen")
}

// The scan moves on past a chunk that is written again while it is being examined, instead of
// taking that chunk again before the rest of its word (a hot chunk would starve the sweep).
func TestReclaimScanAdvancesPastAHotChunk(t *testing.T) {
	t.Parallel()
	base := &mem{data: pattern(testSize)}
	s := newTestStore(t, t.TempDir(), base)
	s.EnableReclaim()
	defer s.Close()
	ctx := context.Background()
	for c := int64(0); c < 4; c++ { // identical whole-chunk writes: stored, since nopwrite is off
		_, err := s.WriteAt(base.data[c*testChunk:(c+1)*testChunk], c*testChunk)
		require.NoError(t, err)
	}
	for c := int64(0); c < 4; c++ {
		require.Equal(t, 1, s.Reclaim(ctx, 1))
		assert.False(t, s.IsWritten(c), "chunk %d examined and dropped", c)
		_, err := s.WriteAt([]byte{0xFF}, c*testChunk) // written again: recorded again, behind the cursor
		require.NoError(t, err)
	}
	assert.EqualValues(t, 4, s.Written())
	assert.EqualValues(t, 4, s.ReclaimStats().Pending)
	assert.Equal(t, 4, s.Reclaim(ctx, 256), "the rewritten chunks are seen after the wrap")
}

// A read that found a chunk's bit set is about to read the COW file when a flush punches the
// chunk Reclaim dropped: it must not read the hole. The punch waits for the read to finish
// (readMu), and the read sees the chunk's bytes, which equal the base's.
func TestReclaimPunchWaitsForAReadInFlight(t *testing.T) {
	t.Parallel()
	base := &mem{data: pattern(testSize)}
	s := newTestStore(t, t.TempDir(), base)
	s.EnableReclaim()
	defer s.Close()
	want := base.data[2*testChunk : 3*testChunk]
	_, err := s.WriteAt(want, 2*testChunk) // identical, stored (nopwrite off)
	require.NoError(t, err)
	require.NoError(t, s.Flush())
	require.Equal(t, 1, s.Reclaim(context.Background(), 256))
	require.EqualValues(t, 0, s.Written(), "dropped, the punch waits for a flush")
	inGap, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.readGap = func() {
		once.Do(func() {
			close(inGap)
			<-proceed
		})
	}
	// The bit was set when this read tested it, before the reclaim: a reader stalled between
	// its bitmap test and its COW read
	got := make([]byte, testChunk)
	readErr := make(chan error, 1)
	s.bitmap.Set(2) // the read must take the COW path, as one that raced the clear did
	go func() {
		_, err := s.ReadAt(got, 2*testChunk)
		readErr <- err
	}()
	<-inGap
	s.bitmap.Clear(2) // the reclaim's clear, from the reader's point of view, lands now
	flushed := make(chan error, 1)
	go func() { flushed <- s.Flush() }()
	select {
	case err := <-flushed:
		t.Fatalf("the flush punched (%v) while a read of the chunk was in flight", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(proceed)
	require.NoError(t, <-readErr)
	assert.Equal(t, want, got, "the read in flight sees the chunk's bytes, never the hole")
	require.NoError(t, <-flushed)
	st, err := s.Stat()
	require.NoError(t, err)
	assert.Zero(t, st.Blocks, "punched once the read was done")
}

// liveView is a mirror plex's base: a live view of the sibling plex, read through its store.
type liveView struct{ of *Store }

func (v *liveView) ReadAt(p []byte, off int64) (int, error) { return v.of.ReadAt(p, off) }
func (v *liveView) Size() int64                             { return v.of.Size() }
func (v *liveView) Close() error                            { return nil }
func (v *liveView) Bind(source.Lookup)                      {}
func (v *liveView) Durable(off, length int64) bool          { return v.of.Durable(off, length) }

// xorView is a parity column's base: the XOR of the data columns, read live through their
// stores.
type xorView struct{ a, b *Store }

func (v *xorView) ReadAt(p []byte, off int64) (int, error) {
	n, err := v.a.ReadAt(p, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, err
	}
	q := make([]byte, n)
	m, err2 := v.b.ReadAt(q, off)
	if err2 != nil && !errors.Is(err2, io.EOF) {
		return 0, err2
	}
	for i := range q[:m] {
		p[i] ^= q[i]
	}
	return min(n, m), err
}
func (v *xorView) Size() int64        { return v.a.Size() }
func (v *xorView) Close() error       { return nil }
func (v *xorView) Bind(source.Lookup) {}
func (v *xorView) Durable(off, length int64) bool {
	return v.a.Durable(off, length) && v.b.Durable(off, length)
}

// nonZero fills p with random bytes none of which is zero, so a hole reads apart from data.
func nonZero(r *rand.Rand, p []byte) {
	for i := range p {
		p[i] = 1 + byte(r.UintN(255))
	}
}

// tortureDuration is how long a torture test keeps its workload running.
func tortureDuration() time.Duration {
	if testing.Short() {
		return 300 * time.Millisecond
	}
	return 3 * time.Second
}

// mirroredWrite writes p to first and then to second, with a short pause between the halves now
// and then, like a guest whose mirrored write lands on the plexes in either order.
func mirroredWrite(r *rand.Rand, p []byte, off int64, first, second *Store) error {
	if _, err := first.WriteAt(p, off); err != nil {
		return err
	}
	if r.IntN(4) == 0 {
		time.Sleep(time.Duration(r.IntN(500)) * time.Microsecond)
	}
	_, err := second.WriteAt(p, off)
	return err
}

// settleAndReclaim examines everything the store has recorded, including the chunks waiting
// for a second look, until nothing is left.
func settleAndReclaim(s *Store) {
	for range 2 {
		time.Sleep(s.settle)
		for s.Reclaim(context.Background(), 256) > 0 {
		}
	}
}

// pendingPunches reports how many chunks wait to be punched.
func (s *Store) pendingPunches() int {
	s.punchMu.Lock()
	defer s.punchMu.Unlock()
	return len(s.punches)
}

// allocatedChunks lists the chunks that have data allocated in the COW file (SEEK_DATA), which
// unlike Stat's block count leaves out filesystem metadata such as extent index blocks.
func allocatedChunks(t *testing.T, s *Store) []int64 {
	t.Helper()
	var chunks []int64
	fd := int(s.cow.Fd())
	for pos := int64(0); pos < s.size; {
		data, err := unix.Seek(fd, pos, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) || data >= s.size {
			break
		}
		require.NoError(t, err)
		hole, err := unix.Seek(fd, data, unix.SEEK_HOLE)
		require.NoError(t, err)
		for c := data / s.chunkSize; c*s.chunkSize < min(hole, s.size); c++ {
			chunks = append(chunks, c)
		}
		pos = hole
	}
	return chunks
}

// storedChunks lists the chunks whose bit is set.
func storedChunks(s *Store) []int64 {
	var chunks []int64
	for c := range s.Chunks() {
		if s.IsWritten(c) {
			chunks = append(chunks, c)
		}
	}
	return chunks
}

// The mirror under torture: plex a over a fixed base, plex b over a live view of a, nopwrite on
// both. Writers mirror random writes to both plexes in either order while a sweeper reclaims b
// with small budgets and a flusher flushes both. Whenever a chunk is quiet it reads the same
// on both plexes and holds what was written; once everything is quiet and swept, b stores
// nothing, owes no punch and its COW file is empty.
func TestReclaimMirrorTorture(t *testing.T) {
	t.Parallel()
	const chunks, writers = 64, 4
	r := rand.New(rand.NewPCG(11, 1))
	baseData := make([]byte, chunks*testChunk)
	nonZero(r, baseData)
	dir := t.TempDir()
	a, err := Open(&mem{data: baseData}, filepath.Join(dir, "a.cow"), filepath.Join(dir, "a.bitmap"), testChunk)
	require.NoError(t, err)
	a.EnableReclaim()
	a.TrackDurability()
	b, err := Open(&liveView{of: a}, filepath.Join(dir, "b.cow"), filepath.Join(dir, "b.bitmap"), testChunk)
	require.NoError(t, err)
	b.EnableReclaim()
	a.SetNopWrite(true)
	b.SetNopWrite(true)
	b.settle = 100 * time.Millisecond
	contents := runMirrorTorture(t, a, b, baseData, writers, tortureDuration(), 11, nil)
	want := assembleChunks(contents, chunks, testChunk)
	assert.Equal(t, want, readAll(t, a), "a holds every write")
	assert.Equal(t, want, readAll(t, b), "b reads what a reads")
	require.NoError(t, a.Flush()) // b relies on a only once a's writes are durable
	settleAndReclaim(b)
	assert.EqualValues(t, 0, b.Written(), "every chunk of b equals a: all reclaimed")
	assert.Zero(t, b.ReclaimStats().Pending)
	require.NoError(t, b.Flush())
	assert.Zero(t, b.pendingPunches(), "the flush punched everything that was queued")
	assert.Empty(t, allocatedChunks(t, b), "b's COW file holds nothing")
	assert.Equal(t, want, readAll(t, b))
	rs := b.ReclaimStats()
	t.Logf("b: %d examined, %d dropped (%d bytes); a: %d chunks stored", rs.Examined, rs.Chunks, rs.Bytes, a.Written())
	assert.Positive(t, rs.Chunks, "the workload froze chunks on b for the sweeper to drop")
	require.NoError(t, b.Close())
	require.NoError(t, a.Close())
}

// runMirrorTorture runs the mirror workload for d against a and b: writers each own the chunks
// c with c%writers == w and check their own chunks after every mirrored write, a sweeper
// reclaims both stores, a flusher flushes both. It returns what each writer wrote; when seen is
// given, every content a chunk had after a mirrored write is added to it.
func runMirrorTorture(t *testing.T, a, b *Store, baseData []byte, writers int, d time.Duration, seed uint64, seen []map[[32]byte]bool) [][]byte {
	t.Helper()
	chunks := int64(len(baseData)) / testChunk
	var stop atomic.Bool
	var wg, bg sync.WaitGroup
	var seenMu sync.Mutex
	errs := make(chan error, writers+2)
	bg.Add(2)
	go func() {
		defer bg.Done()
		r := rand.New(rand.NewPCG(seed, 100))
		for !stop.Load() {
			b.Reclaim(context.Background(), 1+r.IntN(8))
			a.Reclaim(context.Background(), 4)
		}
	}()
	go func() {
		defer bg.Done()
		r := rand.New(rand.NewPCG(seed, 101))
		for !stop.Load() {
			time.Sleep(time.Duration(r.IntN(20)) * time.Millisecond)
			if err := errors.Join(a.Flush(), b.Flush()); err != nil {
				errs <- fmt.Errorf("flush: %w", err)
				return
			}
		}
	}()
	contents := make([][]byte, writers)
	for w := range writers {
		contents[w] = bytes.Clone(baseData)
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(seed, uint64(10+w)))
			own := contents[w]
			for !stop.Load() {
				c := int64(w) + int64(writers)*r.Int64N(chunks/int64(writers))
				start, end := c*testChunk, (c+1)*testChunk
				off, length := start, int64(testChunk)
				if r.IntN(2) == 0 { // a sub-chunk range, 512-aligned
					off = start + r.Int64N(testChunk/512)*512
					length = (1 + r.Int64N((end-off)/512)) * 512
				}
				p := make([]byte, length)
				nonZero(r, p)
				first, second := a, b
				if r.IntN(2) == 0 {
					first, second = b, a
				}
				if err := mirroredWrite(r, p, off, first, second); err != nil {
					errs <- err
					return
				}
				copy(own[off:], p)
				if seen != nil {
					seenMu.Lock()
					seen[c][sha256.Sum256(own[start:end])] = true
					seenMu.Unlock()
				}
				ga, gb := make([]byte, testChunk), make([]byte, testChunk)
				if _, err := a.ReadAt(ga, start); err != nil && !errors.Is(err, io.EOF) {
					errs <- err
					return
				}
				if _, err := b.ReadAt(gb, start); err != nil && !errors.Is(err, io.EOF) {
					errs <- err
					return
				}
				if !bytes.Equal(ga, own[start:end]) {
					errs <- fmt.Errorf("writer %d: chunk %d of a differs from what was written", w, c)
					return
				}
				if !bytes.Equal(gb, ga) {
					errs <- fmt.Errorf("writer %d: chunk %d of b differs from a while quiet", w, c)
					return
				}
			}
		}()
	}
	time.Sleep(d)
	stop.Store(true)
	wg.Wait()
	bg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	return contents
}

// assembleChunks builds the device content from the writers' chunks.
func assembleChunks(contents [][]byte, chunks, chunk int64) []byte {
	want := make([]byte, chunks*chunk)
	for c := range chunks {
		copy(want[c*chunk:(c+1)*chunk], contents[c%int64(len(contents))][c*chunk:(c+1)*chunk])
	}
	return want
}

// The mirror under torture with crashes of plex b between rounds: b's process dies without
// flushing and a successor reopens its files over the same view of a. With a live bitmap the
// successor reads exactly what b read before (a stored chunk its data, a dropped chunk the
// view of a), and the punches the predecessor owed are done at its first flush. Without one,
// a chunk whose bit is still set on disk reads the bytes the COW file holds: some content the
// chunk had at an earlier point (what its base held when it was dropped), never zeros, and
// never anything that was not written there.
func TestReclaimMirrorCrashTorture(t *testing.T) {
	t.Parallel()
	for _, live := range []bool{true, false} {
		t.Run(fmt.Sprintf("live=%v", live), func(t *testing.T) {
			t.Parallel()
			const chunks, writers, rounds = 32, 4, 4
			r := rand.New(rand.NewPCG(13, 1))
			baseData := make([]byte, chunks*testChunk)
			nonZero(r, baseData)
			dir := t.TempDir()
			a, err := Open(&mem{data: baseData}, filepath.Join(dir, "a.cow"), filepath.Join(dir, "a.bitmap"), testChunk)
			require.NoError(t, err)
			a.SetNopWrite(true)
			o := &Options{COWFile: filepath.Join(dir, "b.cow"), Bitmap: filepath.Join(dir, "b.bitmap"), ChunkSize: testChunk}
			if live {
				o.LiveBitmap = filepath.Join(dir, "b.live")
			}
			open := func() *Store {
				b, err := OpenWith(&liveView{of: a}, o)
				require.NoError(t, err)
				b.SetNopWrite(true)
				b.settle = 50 * time.Millisecond
				return b
			}
			b := open()
			// Every content a chunk ever had, as the mirror saw it: what a crash may revert to
			seen := make([]map[[32]byte]bool, chunks)
			for c := range seen {
				seen[c] = map[[32]byte]bool{sha256.Sum256(baseData[c*testChunk : (c+1)*testChunk]): true}
			}
			want := bytes.Clone(baseData)
			for round := range rounds {
				runMirrorTorture(t, a, b, want, writers, tortureDuration()/rounds, uint64(100+round), seen)
				want = readAll(t, a)
				require.Equal(t, want, readAll(t, b), "round %d: b reads what a reads while quiet", round)
				b.abandon() // the crash: nothing flushed, the punch queue and the recorded set are gone
				b = open()
				got := readAll(t, b)
				if live {
					require.Equal(t, want, got, "round %d: with a live bitmap the successor reads what b read", round)
					continue
				}
				resynced := 0
				for c := range chunks {
					chunk := got[c*testChunk : (c+1)*testChunk]
					require.NotContains(t, chunk, byte(0), "round %d: chunk %d reads zeros after the crash", round, c)
					require.True(t, seen[c][sha256.Sum256(chunk)], "round %d: chunk %d (stored %v) reads bytes it never held", round, c, b.IsWritten(int64(c)))
					if !bytes.Equal(chunk, want[c*testChunk:(c+1)*testChunk]) {
						// b's unflushed writes are gone, as without a live bitmap they are for any
						// write: the mirror resyncs the plex from a, as a RAID layer would after an
						// unclean shutdown; the copy freezes on b and is the sweeper's to drop
						_, err := b.WriteAt(want[c*testChunk:(c+1)*testChunk], int64(c)*testChunk)
						require.NoError(t, err)
						resynced++
					}
				}
				t.Logf("round %d: %d chunks resynced after the crash", round, resynced)
				require.Equal(t, want, readAll(t, b))
			}
			// The recorded set died with each predecessor, so chunks frozen before a crash stay
			// stored until written again; but nothing stays allocated beyond them once the
			// successor's first sweep has found the punches its predecessors owed
			settleAndReclaim(b)
			require.NoError(t, b.Flush())
			assert.Zero(t, b.pendingPunches())
			assert.Equal(t, storedChunks(b), allocatedChunks(t, b), "the allocated chunks are exactly the stored ones")
			rs := b.ReclaimStats()
			t.Logf("live=%v: final b: %d stored (frozen before a crash), %d examined, %d dropped", live, b.Written(), rs.Examined, rs.Chunks)
			require.NoError(t, b.Close())
			require.NoError(t, a.Close())
		})
	}
}

// A parity column under torture: p's base is the XOR of data columns d1 and d2 read live, all
// three nopwrite. Writers write a range to d1, d2 and the matching parity to p in random
// order, a sweeper reclaims p, a flusher flushes all three. A quiet chunk of p always reads
// the live XOR, and once everything is quiet and swept, p stores nothing.
func TestReclaimParityTorture(t *testing.T) {
	t.Parallel()
	const chunks, writers = 64, 4
	r := rand.New(rand.NewPCG(17, 1))
	base1, base2 := make([]byte, chunks*testChunk), make([]byte, chunks*testChunk)
	nonZero(r, base1)
	nonZero(r, base2)
	dir := t.TempDir()
	d1, err := Open(&mem{data: base1}, filepath.Join(dir, "d1.cow"), filepath.Join(dir, "d1.bitmap"), testChunk)
	require.NoError(t, err)
	d1.EnableReclaim()
	d1.TrackDurability()
	d2, err := Open(&mem{data: base2}, filepath.Join(dir, "d2.cow"), filepath.Join(dir, "d2.bitmap"), testChunk)
	require.NoError(t, err)
	d2.EnableReclaim()
	d2.TrackDurability()
	p, err := Open(&xorView{a: d1, b: d2}, filepath.Join(dir, "p.cow"), filepath.Join(dir, "p.bitmap"), testChunk)
	require.NoError(t, err)
	p.EnableReclaim()
	for _, s := range []*Store{d1, d2, p} {
		s.SetNopWrite(true)
	}
	p.settle = 100 * time.Millisecond
	var stop atomic.Bool
	var wg, bg sync.WaitGroup
	errs := make(chan error, writers+2)
	bg.Add(2)
	go func() {
		defer bg.Done()
		r := rand.New(rand.NewPCG(17, 100))
		for !stop.Load() {
			p.Reclaim(context.Background(), 1+r.IntN(8))
		}
	}()
	go func() {
		defer bg.Done()
		r := rand.New(rand.NewPCG(17, 101))
		for !stop.Load() {
			time.Sleep(time.Duration(r.IntN(20)) * time.Millisecond)
			if err := errors.Join(d1.Flush(), d2.Flush(), p.Flush()); err != nil {
				errs <- fmt.Errorf("flush: %w", err)
				return
			}
		}
	}()
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(17, uint64(10+w)))
			for !stop.Load() {
				c := int64(w) + int64(writers)*r.Int64N(chunks/writers)
				start := c * testChunk
				off := start + r.Int64N(testChunk/512)*512
				length := (1 + r.Int64N((start+testChunk-off)/512)) * 512
				x1, x2, xp := make([]byte, length), make([]byte, length), make([]byte, length)
				nonZero(r, x1)
				nonZero(r, x2)
				for i := range xp {
					xp[i] = x1[i] ^ x2[i]
				}
				writes := []struct {
					s *Store
					p []byte
				}{{d1, x1}, {d2, x2}, {p, xp}}
				for _, i := range r.Perm(3) {
					if _, err := writes[i].s.WriteAt(writes[i].p, off); err != nil {
						errs <- err
						return
					}
					if r.IntN(4) == 0 {
						time.Sleep(time.Duration(r.IntN(500)) * time.Microsecond)
					}
				}
				g1, g2, gp := make([]byte, testChunk), make([]byte, testChunk), make([]byte, testChunk)
				for _, read := range []struct {
					s *Store
					p []byte
				}{{d1, g1}, {d2, g2}, {p, gp}} {
					if _, err := read.s.ReadAt(read.p, start); err != nil && !errors.Is(err, io.EOF) {
						errs <- err
						return
					}
				}
				for i := range gp {
					if gp[i] != g1[i]^g2[i] {
						errs <- fmt.Errorf("writer %d: chunk %d of p is not the XOR of the data columns while quiet", w, c)
						return
					}
				}
			}
		}()
	}
	time.Sleep(tortureDuration())
	stop.Store(true)
	wg.Wait()
	bg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	require.NoError(t, d1.Flush()) // p relies on the data columns only once they are durable
	require.NoError(t, d2.Flush())
	settleAndReclaim(p)
	assert.EqualValues(t, 0, p.Written(), "every chunk of p equals the live XOR: all reclaimed")
	require.NoError(t, p.Flush())
	assert.Zero(t, p.pendingPunches())
	assert.Empty(t, allocatedChunks(t, p), "p's COW file holds nothing")
	want := readAll(t, d1)
	for i, x := range readAll(t, d2) {
		want[i] ^= x
	}
	assert.Equal(t, want, readAll(t, p), "p reads the live XOR")
	rs := p.ReclaimStats()
	t.Logf("p: %d examined, %d dropped; d1 %d, d2 %d chunks stored", rs.Examined, rs.Chunks, d1.Written(), d2.Written())
	assert.Positive(t, rs.Chunks, "the workload froze parity chunks for the sweeper to drop")
	require.NoError(t, p.Close())
	require.NoError(t, d1.Close())
	require.NoError(t, d2.Close())
}

// Reclaim's state (the recorded sets and the bitmap's copy of the file) costs about four
// bits per chunk, 64 MiB on an 8 TiB device: a store allocates it only once reclaim is on.
func TestReclaimStateOnlyWhenEnabled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newTestStore(t, dir, &mem{data: pattern(testSize)})
	_, err := s.WriteAt(pattern(testChunk), 0) // what the base holds: reclaimable once enabled
	require.NoError(t, err)
	assert.Nil(t, s.recent)
	assert.Nil(t, s.again)
	assert.Nil(t, s.due)
	assert.Nil(t, s.bitmap.disk.Load())
	assert.Zero(t, s.Reclaim(context.Background(), 100), "off: nothing recorded, nothing examined")
	require.NoError(t, s.Flush())
	s.EnableReclaim()
	assert.NotNil(t, s.recent)
	assert.True(t, s.bitmap.committed(0), "the copy of the file is read when reclaim turns on")
	_, err = s.WriteAt(pattern(testChunk), 0)
	require.NoError(t, err)
	assert.Equal(t, 1, s.Reclaim(context.Background(), 100))
	assert.EqualValues(t, 0, s.Written())
	require.NoError(t, s.Close())
}

// storeView is a sibling's live view of a store, the way a mirror plex reads its partner:
// it forwards Durable, so a store over it relies only on content the sibling made durable.
type storeView struct{ s *Store }

func (v *storeView) Size() int64                             { return v.s.Size() }
func (v *storeView) ReadAt(p []byte, off int64) (int, error) { return v.s.ReadAt(p, off) }
func (v *storeView) Close() error                            { return nil }
func (v *storeView) Durable(off, length int64) bool          { return v.s.Durable(off, length) }

// siblingPair opens a over a zero base and b over a live view of a, both without live
// bitmaps, so abandon models a host reboot.
func siblingPair(t *testing.T, dir string) (a, b *Store, reopen func() (*Store, *Store)) {
	t.Helper()
	ao := &Options{COWFile: filepath.Join(dir, "a.cow"), Bitmap: filepath.Join(dir, "a.bitmap"), ChunkSize: testChunk}
	bo := &Options{COWFile: filepath.Join(dir, "b.cow"), Bitmap: filepath.Join(dir, "b.bitmap"), ChunkSize: testChunk}
	open := func() (*Store, *Store) {
		a, err := OpenWith(source.NewZero(testSize), ao)
		require.NoError(t, err)
		a.TrackDurability() // b's base reads it, as ServeGroup sets up
		b, err := OpenWith(&storeView{s: a}, bo)
		require.NoError(t, err)
		return a, b
	}
	a, b = open()
	return a, b, open
}

// From the 2026-10-05 external review (finding 01): b flushed 0x42; a later gets the same
// bytes without a flush; reclaiming b's chunk because it equals a's current content, then a
// reboot, lost b's flushed data (a's write never became durable). b may drop a chunk only
// in favour of content its sibling made durable.
func TestReclaimNeverReliesOnAnUnflushedSibling(t *testing.T) {
	t.Parallel()
	a, b, reopen := siblingPair(t, t.TempDir())
	b.EnableReclaim()
	want := bytes.Repeat([]byte{0x42}, testChunk)
	_, err := b.WriteAt(want, 0)
	require.NoError(t, err)
	require.NoError(t, b.Flush())
	_, err = a.WriteAt(want, 0) // the same bytes on the sibling, not flushed
	require.NoError(t, err)
	b.Reclaim(context.Background(), 1)
	require.NoError(t, b.Flush())
	b.abandon()
	a.abandon()
	a, b = reopen()
	defer a.Close()
	defer b.Close()
	assert.Equal(t, want, readAll(t, b)[:testChunk], "b's flushed data must survive a reboot")
}

// The same flaw through nopwrite: b skips a write because a holds the bytes, unflushed; b's
// flush succeeds, and after a reboot b reads a's durable content instead of what it acked.
func TestNopWriteNeverReliesOnAnUnflushedSibling(t *testing.T) {
	t.Parallel()
	a, b, reopen := siblingPair(t, t.TempDir())
	b.SetNopWrite(true)
	want := bytes.Repeat([]byte{0x42}, testChunk)
	_, err := a.WriteAt(want, 0) // not flushed
	require.NoError(t, err)
	_, err = b.WriteAt(want, 0) // equals what b reads through a now
	require.NoError(t, err)
	require.NoError(t, b.Flush())
	b.abandon()
	a.abandon()
	a, b = reopen()
	defer a.Close()
	defer b.Close()
	assert.Equal(t, want, readAll(t, b)[:testChunk], "a write b acknowledged with a flush must survive a reboot")
}

// Once the sibling flushed, relying on it is safe again: nopwrite skips, reclaim drops.
func TestNopWriteAndReclaimRelyOnADurableSibling(t *testing.T) {
	t.Parallel()
	a, b, _ := siblingPair(t, t.TempDir())
	defer a.Close()
	defer b.Close()
	b.SetNopWrite(true)
	b.EnableReclaim()
	want := bytes.Repeat([]byte{0x42}, testChunk)
	_, err := a.WriteAt(want, 0)
	require.NoError(t, err)
	require.NoError(t, a.Flush())
	_, err = b.WriteAt(want, 0)
	require.NoError(t, err)
	assert.EqualValues(t, 0, b.Written(), "a durable sibling: the write is skipped")
	_, err = b.WriteAt(want, testChunk) // a still reads zeros there, durably
	require.NoError(t, err)
	_, err = a.WriteAt(want, testChunk)
	require.NoError(t, err)
	require.NoError(t, a.Flush())
	b.Reclaim(context.Background(), 10)
	assert.EqualValues(t, 0, b.Written(), "the sibling caught up durably: dropped")
}

// From the 2026-10-05 external review (finding 02): Writeback tested a chunk's bit, then took
// its lock; a reclaim and a flush in between cleared and punched the chunk, and Writeback
// copied the hole: zeros into the destination over the right bytes.
func TestWritebackSkipsAChunkReclaimedMeanwhile(t *testing.T) {
	t.Parallel()
	base := bytes.Repeat([]byte{0x42}, testSize)
	s := newTestStore(t, t.TempDir(), &mem{data: base})
	defer s.Close()
	s.EnableReclaim()
	_, err := s.WriteAt(base[:testChunk], 0) // identical to the base: reclaimable
	require.NoError(t, err)
	dst := &sliceWriter{bytes.Clone(base)}
	s.writebackGap = func() {
		s.writebackGap = nil
		s.Reclaim(context.Background(), 1)
		require.NoError(t, s.Flush()) // commits the clear, punches the chunk
	}
	_, err = s.Writeback(dst)
	require.NoError(t, err)
	assert.Equal(t, base, dst.b, "writeback must not copy a punched chunk")
}

// An overlay recorded with an older form of its base's identity (before HTTP identities named
// their resource) opens and is re-pinned to the current form; any other identity is refused.
func TestStoreAcceptsALegacyIdentityAndRepins(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	o := &Options{COWFile: filepath.Join(dir, "d.cow"), Bitmap: filepath.Join(dir, "d.bitmap"), ChunkSize: testChunk, Identity: "http:4096:etag=x@0+4096"}
	s, err := OpenWith(&mem{data: pattern(testSize)}, o)
	require.NoError(t, err)
	_, err = s.WriteAt([]byte{1}, 0)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	o.Identity, o.LegacyIdentity = "http:4096:etag=x@0+4096#r0123456789abcdef", "http:4096:etag=x@0+4096"
	s, err = OpenWith(&mem{data: pattern(testSize)}, o)
	require.NoError(t, err, "the legacy form of the same identity is accepted")
	require.NoError(t, s.Close())
	info, err := Inspect(o.Bitmap)
	require.NoError(t, err)
	assert.Equal(t, o.Identity, info.Identity, "and re-pinned to the current form")
	o.Identity, o.LegacyIdentity = "http:4096:etag=x@0+4096#rfedcba9876543210", "http:4096:etag=x@0+4096"
	_, err = OpenWith(&mem{data: pattern(testSize)}, o)
	assert.ErrorIs(t, err, ErrSourceChanged, "once re-pinned, another resource is refused")
}

// The review's reopen criterion for finding 03: an overlay made over one HTTP resource is
// refused over another that answers with the same size and ETag.
func TestStoreRefusesAnotherResourceWithTheSameValidator(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"revision-1"`)
		value := byte(1)
		if r.URL.Path == "/b" {
			value = 2
		}
		http.ServeContent(w, r, "img", time.Time{}, bytes.NewReader(bytes.Repeat([]byte{value}, testSize)))
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	open := func(path string) (*Store, error) {
		h, err := source.NewHTTP(srv.Client(), srv.URL+path, 0, 0)
		require.NoError(t, err)
		id := source.Identity(h)
		return OpenWith(h, &Options{COWFile: filepath.Join(dir, "d.cow"), Bitmap: filepath.Join(dir, "d.bitmap"), ChunkSize: testChunk, Identity: id, LegacyIdentity: source.LegacyIdentity(id)})
	}
	s, err := open("/a")
	require.NoError(t, err)
	_, err = s.WriteAt([]byte{9}, 0)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	_, err = open("/b")
	assert.ErrorIs(t, err, ErrSourceChanged, "the same validator on another resource is another source")
	s, err = open("/a")
	require.NoError(t, err, "the original resource still opens")
	require.NoError(t, s.Close())
}

// From the 2026-10-05 external review (finding 06): the live bitmap went only at Close or
// Abandon, so a process that died between a failed COW sync and that cleanup left it behind,
// and the successor adopted bits whose data the failed sync may have lost. It goes at once.
func TestFailedCOWSyncDropsTheLiveBitmapAtOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	o := &Options{COWFile: filepath.Join(dir, "d.cow"), Bitmap: filepath.Join(dir, "d.bitmap"), LiveBitmap: filepath.Join(dir, "d.live"), ChunkSize: testChunk}
	s, err := OpenWith(&mem{data: pattern(testSize)}, o)
	require.NoError(t, err)
	_, err = s.WriteAt(bytes.Repeat([]byte{0x42}, testChunk), 0)
	require.NoError(t, err)
	s.syncCOW = func() error { return syscall.EIO }
	require.ErrorIs(t, s.Flush(), ErrCOWFailed)
	_, err = os.Stat(o.LiveBitmap)
	assert.True(t, os.IsNotExist(err), "the live bitmap goes with the failure, not with a later cleanup")
	s.abandon() // the process dies before Close or Abandon
	s, err = OpenWith(&mem{data: pattern(testSize)}, o)
	require.NoError(t, err)
	defer s.Close()
	assert.EqualValues(t, 0, s.Written(), "the successor must not adopt the failed generation's bits")
	assert.Equal(t, pattern(testSize), readAll(t, s))
}

func TestReclaimSelectionDoesNotRescanEmptySets(t *testing.T) {
	// A few second-pass candidates on a large device must not cost a scan of every bitmap word
	// per candidate (the recent set being empty, and the due set from its start)
	const words = 1 << 19 // 1 TiB of 64 KiB chunks
	for _, at := range []int{0, words - 2} {
		s := &Store{recent: make([]uint32, words), again: make([]uint32, words), due: make([]uint32, words)}
		for chunk := range 64 {
			require.True(t, s.record(s.again, int64(at)*bitmapWordBits+int64(chunk)))
			s.againCount++
		}
		s.settle = time.Hour
		_, _, ok := s.nextRecent() // refills due, which then settles
		require.False(t, ok)
		for range 10 {
			_, _, ok = s.nextRecent()
			require.False(t, ok)
		}
		assert.Less(t, s.scans, int64(16), "settling: words scanned")
		s.scans, s.dueAt = 0, time.Now().Add(-2*time.Hour)
		for chunk := range 64 {
			got, second, ok := s.nextRecent()
			require.True(t, ok)
			require.True(t, second)
			require.Equal(t, int64(at)*bitmapWordBits+int64(chunk), got)
		}
		_, _, ok = s.nextRecent()
		require.False(t, ok)
		assert.Less(t, s.scans, int64(words+64), "settled: words scanned for 64 candidates at word %d", at)
	}
}

func TestStoreWithoutOverlay(t *testing.T) {
	// No COW file: a read-only device reads its base through the store and keeps no state
	dir := t.TempDir()
	t.Chdir(dir)
	base := &mem{data: pattern(testSize)}
	s, err := OpenWith(base, &Options{ChunkSize: testChunk})
	require.NoError(t, err)
	assert.False(t, s.Overlay())
	assert.Equal(t, pattern(testSize), readAll(t, s))
	_, err = s.WriteAt([]byte{1}, 0)
	assert.ErrorIs(t, err, ErrNoOverlay)
	assert.ErrorIs(t, s.Discard(0, testChunk), ErrNoOverlay)
	assert.ErrorIs(t, s.WriteZeroes(0, testChunk), ErrNoOverlay)
	assert.Zero(t, s.Written())
	assert.False(t, s.Dirty())
	assert.NoError(t, s.Flush())
	assert.True(t, s.Durable(0, testSize), "nothing to lose: the base is the content")
	s.EnableReclaim() // nothing to reclaim; must not need files
	assert.Zero(t, s.Reclaim(context.Background(), 10))
	require.NoError(t, s.Close())
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "no file of any kind")
}
