// Package cow layers a copy-on-write overlay over a read-only source. Writes land in a
// sparse COW file at their device offset; a bitmap records which chunks live there.
package cow

import (
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os"
	"sync"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"

	"heckel.io/blkmap/source"
)

const (
	// lockStripes bounds the per-chunk mutexes; chunks share a stripe by index modulo.
	lockStripes = 1024
	cowFileMode = 0600
	// MaxRunBytes is the largest HydrateRun served from a pooled buffer; the hydrator sizes
	// its runs to it.
	MaxRunBytes = 1 << 20
)

var (
	errOutOfRange = errors.New("range beyond device end")
)

// Info is what Inspect reads from a bitmap file.
type Info struct {
	Size      int64 // device size the bitmap was created for
	ChunkSize int64
	Chunks    int64
	Written   int64 // chunks recorded as present in the COW file
}

// Store is the writable device image: reads come from the COW file for written chunks and
// from the base source otherwise. It implements ublk.Backend.
type Store struct {
	base      source.Source
	cow       *os.File
	bitmap    *Bitmap
	chunkSize int64
	size      int64
	dirty     atomic.Bool             // something changed since the last Flush
	bufs      sync.Pool               // chunk-sized scratch buffers for read-modify-write
	runBufs   sync.Pool               // MaxRunBytes buffers for hydration runs
	flushMu   sync.Mutex              // Serializes Flush, whose data-then-bitmap order must not interleave
	locks     [lockStripes]sync.Mutex // Serializes read-modify-write per chunk stripe
}

// Open opens or creates the COW file and bitmap for base. The Store takes ownership of base.
// The COW file is locked so a second server cannot corrupt it, and a bitmap that records
// written chunks refuses a COW file that is missing or shorter than the device: the data it
// describes would read as zeros.
func Open(base source.Source, cowPath, bitmapPath string, chunkSize int64) (*Store, error) {
	size := base.Size()
	cow, err := os.OpenFile(cowPath, os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, cowFileMode)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Store, error) {
		cow.Close()
		return nil, err
	}
	if err := unix.Flock(int(cow.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fail(fmt.Errorf("cow file %s is in use by another process", cowPath))
	}
	st, err := cow.Stat()
	if err != nil {
		return fail(err)
	}
	bitmap, err := OpenBitmap(bitmapPath, size, chunkSize)
	if err != nil {
		return fail(err)
	}
	if written := bitmap.Count(); written > 0 && st.Size() < size {
		bitmap.CloseNoSync()
		return fail(fmt.Errorf("cow file %s is missing or truncated (%d bytes) but its bitmap records %d written chunks; restore it or delete the bitmap to start over", cowPath, st.Size(), written))
	}
	// Extend a fresh COW file to the device size so it is a complete sparse image
	if st.Size() < size {
		if err := cow.Truncate(size); err != nil {
			bitmap.CloseNoSync()
			return fail(fmt.Errorf("cow file %s: %w", cowPath, err))
		}
	}
	s := &Store{base: base, cow: cow, bitmap: bitmap, chunkSize: chunkSize, size: size}
	s.bufs.New = func() any {
		b := make([]byte, chunkSize)
		return &b
	}
	s.runBufs.New = func() any {
		b := make([]byte, MaxRunBytes)
		return &b
	}
	return s, nil
}

func (s *Store) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("%w: negative offset %d", errOutOfRange, off)
	}
	var eof error
	if off >= s.size {
		return 0, io.EOF
	}
	if int64(len(p)) > s.size-off {
		p, eof = p[:s.size-off], io.EOF
	}
	n := len(p)
	// Serve runs of chunks with the same state in one call each, so a request that spans
	// many unwritten chunks reaches the base source once (one round trip for a remote one)
	for len(p) > 0 {
		chunk := off / s.chunkSize
		written := s.bitmap.Test(chunk)
		end := (chunk + 1) * s.chunkSize
		for end < off+int64(len(p)) && s.bitmap.Test(end/s.chunkSize) == written {
			end += s.chunkSize
		}
		m := int(min(int64(len(p)), end-off))
		var err error
		if written {
			_, err = s.cow.ReadAt(p[:m], off)
		} else {
			_, err = s.base.ReadAt(p[:m], off)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return n - len(p), err
		}
		p = p[m:]
		off += int64(m)
	}
	return n, eof
}

func (s *Store) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > s.size {
		return 0, fmt.Errorf("%w: offset %d, length %d, size %d", errOutOfRange, off, len(p), s.size)
	}
	n := len(p)
	for len(p) > 0 {
		chunk := off / s.chunkSize
		m := int(min(int64(len(p)), (chunk+1)*s.chunkSize-off))
		if err := s.writeChunk(chunk, p[:m], off); err != nil {
			return n - len(p), err
		}
		p = p[m:]
		off += int64(m)
	}
	return n, nil
}

func (s *Store) Size() int64 {
	return s.size
}

// Abort makes blocked and future base reads fail at once, if the base supports it, so a
// stopping device never waits on a hung source.
func (s *Store) Abort() {
	source.Abort(s.base)
}

// Flush makes all completed writes durable: the bitmap pages are snapshotted first, then the
// COW data is synced, then the snapshot is written, so a bit on disk never describes data
// that is not (a write landing between the two syncs stays dirty for the next Flush).
func (s *Store) Flush() error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	if !s.dirty.Swap(false) {
		return nil
	}
	pages := s.bitmap.Snapshot()
	if err := s.cow.Sync(); err != nil {
		s.bitmap.Redirty(pages)
		s.dirty.Store(true)
		return err
	}
	if err := s.bitmap.Commit(pages); err != nil {
		s.dirty.Store(true)
		return err
	}
	return nil
}

// Written returns the number of chunks that live in the COW file.
func (s *Store) Written() int64 {
	return s.bitmap.Count()
}

// Close flushes and closes the COW file, the bitmap, and the base source. If the data could
// not be made durable, pending bits are dropped rather than written ahead of it.
func (s *Store) Close() error {
	err := s.Flush()
	closeBitmap := s.bitmap.Close
	if err != nil {
		closeBitmap = s.bitmap.CloseNoSync
	}
	return errors.Join(err, s.cow.Close(), closeBitmap(), s.base.Close())
}

// writeChunk writes p, which lies entirely within chunk, at device offset off. The first
// write to a chunk that does not cover it entirely first copies the chunk from base, so
// the COW file always holds whole chunks and the bitmap stays exact.
func (s *Store) writeChunk(chunk int64, p []byte, off int64) error {
	start := chunk * s.chunkSize
	length := min(s.chunkSize, s.size-start) // the last chunk may be partial
	mu := &s.locks[chunk%lockStripes]
	mu.Lock()
	defer mu.Unlock()
	if !s.bitmap.Test(chunk) && int64(len(p)) < length {
		scratch := s.bufs.Get().(*[]byte)
		defer s.bufs.Put(scratch)
		buf := (*scratch)[:length]
		if _, err := s.base.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("copy chunk %d from base: %w", chunk, err)
		}
		copy(buf[off-start:], p)
		if _, err := s.cow.WriteAt(buf, start); err != nil {
			return err
		}
	} else if _, err := s.cow.WriteAt(p, off); err != nil {
		return err
	}
	s.bitmap.Set(chunk)
	s.dirty.Store(true)
	return nil
}

// Discard drops a range: whole chunks already in the COW file are punched out, so they read
// as zeros and stop using space. Partial chunks and unwritten chunks are left alone, which
// discard semantics allow.
func (s *Store) Discard(off, length int64) error {
	if err := s.checkRange(off, length); err != nil {
		return err
	}
	first := (off + s.chunkSize - 1) / s.chunkSize
	end := (off + length) / s.chunkSize
	for chunk := first; chunk < end; chunk++ {
		mu := &s.locks[chunk%lockStripes]
		mu.Lock()
		var err error
		if s.bitmap.Test(chunk) {
			err = s.punch(chunk)
			s.dirty.Store(true)
		}
		mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

// WriteZeroes zeroes a range: whole chunks are punched out and marked written (reading as
// zeros from the sparse COW file); partial chunks go through the normal write path.
func (s *Store) WriteZeroes(off, length int64) error {
	if err := s.checkRange(off, length); err != nil {
		return err
	}
	var zeros []byte
	for end := off + length; off < end; {
		chunk := off / s.chunkSize
		chunkStart := chunk * s.chunkSize
		chunkEnd := min(chunkStart+s.chunkSize, s.size)
		m := min(end, chunkEnd) - off
		var err error
		if off == chunkStart && m == chunkEnd-chunkStart {
			mu := &s.locks[chunk%lockStripes]
			mu.Lock()
			if err = s.punch(chunk); err == nil {
				s.bitmap.Set(chunk)
				s.dirty.Store(true)
			}
			mu.Unlock()
		} else {
			if int64(len(zeros)) < m {
				zeros = make([]byte, m)
			}
			err = s.writeChunk(chunk, zeros[:m], off)
		}
		if err != nil {
			return err
		}
		off += m
	}
	return nil
}

// punch deallocates chunk in the COW file; it reads back as zeros. Callers hold the lock.
func (s *Store) punch(chunk int64) error {
	start := chunk * s.chunkSize
	length := min(s.chunkSize, s.size-start)
	return unix.Fallocate(int(s.cow.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, start, length)
}

func (s *Store) checkRange(off, length int64) error {
	if off < 0 || length < 0 || off+length > s.size {
		return fmt.Errorf("%w: offset %d, length %d, size %d", errOutOfRange, off, length, s.size)
	}
	return nil
}

// Chunks returns the number of chunks in the device.
func (s *Store) Chunks() int64 {
	return s.bitmap.Chunks()
}

// ChunkSize returns the COW granularity in bytes.
func (s *Store) ChunkSize() int64 {
	return s.chunkSize
}

// IsWritten reports whether chunk lives in the COW file; out-of-range chunks do not.
func (s *Store) IsWritten(chunk int64) bool {
	return s.bitmap.Test(chunk)
}

// MarkZero records chunk as written without copying anything, for chunks known to read as
// zeros: the sparse COW file reads zeros there. It reports whether the bit was newly set.
func (s *Store) MarkZero(chunk int64) bool {
	if chunk < 0 || chunk >= s.bitmap.Chunks() {
		return false
	}
	mu := &s.locks[chunk%lockStripes]
	mu.Lock()
	defer mu.Unlock()
	if s.bitmap.Test(chunk) {
		return false
	}
	s.bitmap.Set(chunk)
	s.dirty.Store(true)
	return true
}

// Stat returns the COW file's metadata (its allocated blocks show how sparse it is).
func (s *Store) Stat() (*syscall.Stat_t, error) {
	fi, err := s.cow.Stat()
	if err != nil {
		return nil, err
	}
	return fi.Sys().(*syscall.Stat_t), nil
}

// Complete reports, from the bitmap file alone, whether every chunk has been written, along
// with the device size and chunk size the bitmap was created for. A missing bitmap is not an
// error: complete is false.
func Complete(bitmapPath string) (size, chunkSize int64, complete bool, err error) {
	info, err := Inspect(bitmapPath)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return info.Size, info.ChunkSize, info.Written == info.Chunks, nil
}

// Inspect reads a bitmap file without opening the store.
func Inspect(bitmapPath string) (*Info, error) {
	data, err := os.ReadFile(bitmapPath)
	if err != nil {
		return nil, err
	}
	size, chunkSize, err := parseHeader(data)
	if err != nil {
		return nil, fmt.Errorf("%w %s: %w", errBitmap, bitmapPath, err)
	}
	info := &Info{Size: size, ChunkSize: chunkSize, Chunks: (size + chunkSize - 1) / chunkSize}
	if int64(len(data)-bitmapHeaderSize)*8 < info.Chunks {
		return nil, fmt.Errorf("%w %s: file too short for %d chunks", errBitmap, bitmapPath, info.Chunks)
	}
	for _, b := range data[bitmapHeaderSize:] {
		info.Written += int64(bits.OnesCount8(b))
	}
	return info, nil
}

// Dirty reports whether anything changed since the last Flush.
func (s *Store) Dirty() bool {
	return s.dirty.Load()
}

// HydrateRun copies the unwritten chunks among [first, first+count) from the base with one
// read, so a remote source sees one request per run instead of one per chunk. It reports the
// bytes copied; chunks the guest wrote meanwhile keep the guest's data.
func (s *Store) HydrateRun(first, count int64, direct bool) (int64, error) {
	chunks := s.bitmap.Chunks()
	if first < 0 || first >= chunks || count <= 0 {
		return 0, fmt.Errorf("%w: chunks %d..%d of %d", errOutOfRange, first, first+count, chunks)
	}
	last := min(first+count, chunks)
	start := first * s.chunkSize
	length := min(last*s.chunkSize, s.size) - start
	buf := make([]byte, length)
	if length <= MaxRunBytes {
		scratch := s.runBufs.Get().(*[]byte)
		defer s.runBufs.Put(scratch)
		buf = (*scratch)[:length]
	}
	var n int
	var err error
	if direct {
		n, err = source.ReadDirect(s.base, buf, start)
	} else {
		n, err = s.base.ReadAt(buf, start)
	}
	if err != nil && !(errors.Is(err, io.EOF) && n == len(buf)) {
		return 0, fmt.Errorf("hydrate chunks %d..%d: %w", first, last, err)
	}
	if n < len(buf) {
		return 0, fmt.Errorf("hydrate chunks %d..%d: short read (%d of %d bytes)", first, last, n, len(buf))
	}
	var copied int64
	for chunk := first; chunk < last; chunk++ {
		off := (chunk - first) * s.chunkSize
		data := buf[off:min(off+s.chunkSize, int64(len(buf)))]
		mu := &s.locks[chunk%lockStripes]
		mu.Lock()
		if !s.bitmap.Test(chunk) {
			if _, err := s.cow.WriteAt(data, start+off); err != nil {
				mu.Unlock()
				return copied, err
			}
			s.bitmap.Set(chunk)
			s.dirty.Store(true)
			copied += int64(len(data))
		}
		mu.Unlock()
	}
	return copied, nil
}
