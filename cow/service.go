// Package cow layers a copy-on-write overlay over a read-only source. Writes land in a
// sparse COW file at their device offset; a bitmap records which chunks live there.
package cow

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"golang.org/x/sys/unix"

	"heckel.io/blkmap/source"
)

const (
	// lockStripes bounds the per-chunk mutexes; chunks share a stripe by index modulo.
	lockStripes = 1024
	cowFileMode = 0600
)

var (
	errOutOfRange = errors.New("range beyond device end")
)

// Store is the writable device image: reads come from the COW file for written chunks and
// from the base source otherwise. It implements the go-ublk Backend interface.
type Store struct {
	base      source.Source
	cow       *os.File
	bitmap    *Bitmap
	chunkSize int64
	size      int64
	locks     [lockStripes]sync.Mutex // Serializes read-modify-write per chunk stripe
}

// Open opens or creates the COW file and bitmap for base. The Store takes ownership of base.
func Open(base source.Source, cowPath, bitmapPath string, chunkSize int64) (*Store, error) {
	size := base.Size()
	bitmap, err := OpenBitmap(bitmapPath, size, chunkSize)
	if err != nil {
		return nil, err
	}
	cow, err := os.OpenFile(cowPath, os.O_RDWR|os.O_CREATE, cowFileMode)
	if err != nil {
		bitmap.Close()
		return nil, err
	}
	// Extend a fresh (or short) COW file to the device size so it is a complete sparse image
	if st, err := cow.Stat(); err != nil {
		cow.Close()
		bitmap.Close()
		return nil, err
	} else if st.Size() < size {
		if err := cow.Truncate(size); err != nil {
			cow.Close()
			bitmap.Close()
			return nil, fmt.Errorf("cow file %s: %w", cowPath, err)
		}
	}
	return &Store{base: base, cow: cow, bitmap: bitmap, chunkSize: chunkSize, size: size}, nil
}

func (s *Store) ReadAt(p []byte, off int64) (int, error) {
	var eof error
	if off >= s.size {
		return 0, io.EOF
	}
	if int64(len(p)) > s.size-off {
		p, eof = p[:s.size-off], io.EOF
	}
	n := len(p)
	for len(p) > 0 {
		chunk := off / s.chunkSize
		m := int(min(int64(len(p)), (chunk+1)*s.chunkSize-off))
		var err error
		if s.bitmap.Test(chunk) {
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

// Flush makes all completed writes durable: COW data first, then the bitmap.
func (s *Store) Flush() error {
	if err := s.cow.Sync(); err != nil {
		return err
	}
	return s.bitmap.Sync()
}

// Written returns the number of chunks that live in the COW file.
func (s *Store) Written() int64 {
	return s.bitmap.Count()
}

// Close flushes and closes the COW file, the bitmap, and the base source.
func (s *Store) Close() error {
	return errors.Join(s.Flush(), s.cow.Close(), s.bitmap.Close(), s.base.Close())
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
		buf := make([]byte, length)
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
