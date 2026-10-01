package source

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// File reads a window of a regular file or block device.
type File struct {
	f      *os.File
	offset int64
	size   int64
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
	return &File{f: f, offset: offset, size: size}, nil
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

func (f *File) Size() int64 {
	return f.size
}

func (f *File) Close() error {
	return f.f.Close()
}
