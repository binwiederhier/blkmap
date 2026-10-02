package source

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// File reads a window of a regular file or block device.
type File struct {
	f      *os.File
	offset int64
	size   int64
	total  int64      // length of the whole file
	seekMu sync.Mutex // Serializes SEEK_HOLE/SEEK_DATA, which move the shared file offset
}

// OpenFile opens a window of size bytes starting at offset within path. A size of 0 means
// everything after offset. Block devices are supported (their length comes from a seek to
// the end, since Stat reports 0 for them).
func OpenFile(path string, offset, size int64) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	total, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: cannot determine length: %w", path, err)
	}
	if size, err = window(offset, size, total); err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &File{f: f, offset: offset, size: size, total: total}, nil
}

func (f *File) ReadAt(p []byte, off int64) (int, error) {
	n, eof := clampRead(len(p), off, f.size)
	read, err := f.f.ReadAt(p[:n], f.offset+off)
	// A short read inside the window is an error (truncated file); at the window's end it
	// is the expected EOF
	if err != nil && !(errors.Is(err, io.EOF) && read == n) {
		return read, err
	}
	return read, eof
}

// Holes reports the file's holes within the range via SEEK_HOLE/SEEK_DATA. Block devices
// and filesystems without hole support report nothing (nil).
func (f *File) Holes(off, length int64) ([]Range, error) {
	if off < 0 || off >= f.size || length <= 0 {
		return nil, nil
	}
	limit := f.offset + min(off+length, f.size)
	f.seekMu.Lock()
	defer f.seekMu.Unlock()
	var holes []Range
	fd := int(f.f.Fd())
	for pos := f.offset + off; pos < limit; {
		hole, err := unix.Seek(fd, pos, unix.SEEK_HOLE)
		if errors.Is(err, unix.ENXIO) {
			break // pos is past the end of the file
		} else if err != nil {
			return nil, nil // no hole support here: say nothing rather than guess
		}
		if hole >= limit {
			break
		}
		data, err := unix.Seek(fd, hole, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			data = f.total // the hole runs to the end of the file
		} else if err != nil {
			return nil, nil
		}
		holes = append(holes, Range{Offset: hole - f.offset, Length: min(data, limit) - hole})
		pos = data
	}
	return holes, nil
}

func (f *File) Size() int64 {
	return f.size
}

func (f *File) Close() error {
	return f.f.Close()
}
