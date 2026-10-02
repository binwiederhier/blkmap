package source

import (
	"fmt"
	"io"
)

// clampRead bounds a read of n bytes at off to a source of the given size, returning the
// number of bytes that can be read and io.EOF when that is fewer than n.
func clampRead(n int, off, size int64) (int, error) {
	if off >= size {
		return 0, io.EOF
	}
	if remaining := size - off; int64(n) > remaining {
		return int(remaining), io.EOF
	}
	return n, nil
}

// window validates an [offset, offset+size) window within total bytes and resolves a zero
// size to "everything after offset".
func window(offset, size, total int64) (int64, error) {
	if offset < 0 || size < 0 {
		return 0, fmt.Errorf("offset and size must not be negative")
	}
	if offset > total {
		return 0, fmt.Errorf("offset %d is beyond the end (%d bytes)", offset, total)
	}
	if size == 0 {
		return total - offset, nil
	}
	if offset+size > total {
		return 0, fmt.Errorf("offset %d + size %d is beyond the end (%d bytes)", offset, size, total)
	}
	return size, nil
}
