package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"heckel.io/blkmap/source"
)

const (
	// blockSize is the unit the cache directory stores: one file per block, named by index.
	blockSize = 1 << 20
	blockName = "%08d.blk"
)

// DirCache is a fast tier backed by a directory of block files: block i of the image lives
// in <dir>/<i>.blk, whole or absent. A read whose block file is missing answers
// source.ErrNotFound, which source.Cache counts as a miss and serves from the slow tier. The
// directory is filled by something else (here: the -populate flag of this example); blkmap
// only reads it.
type DirCache struct {
	dir  string
	size int64
}

func NewDirCache(dir string, size int64) *DirCache {
	return &DirCache{dir: dir, size: size}
}

func (c *DirCache) ReadAt(p []byte, off int64) (int, error) {
	if off >= c.size {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), c.size-off))
	for pos := off; pos < off+int64(n); {
		block := pos / blockSize
		within := pos % blockSize
		m := int(min(int64(n)-(pos-off), blockSize-within))
		f, err := os.Open(c.path(block))
		if errors.Is(err, os.ErrNotExist) {
			return int(pos - off), source.ErrNotFound
		} else if err != nil {
			return int(pos - off), err
		}
		read, err := f.ReadAt(p[pos-off:pos-off+int64(m)], within)
		f.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return int(pos - off), err
		}
		if read < m { // a truncated block file is as good as missing
			return int(pos - off), source.ErrNotFound
		}
		pos += int64(m)
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (c *DirCache) Size() int64 {
	return c.size
}

func (c *DirCache) Close() error {
	return nil
}

// Populate copies blocks [from, to) of src into the cache directory, the way an external
// cache filler would.
func (c *DirCache) Populate(src source.Source, from, to int64) error {
	buf := make([]byte, blockSize)
	for block := from; block < to; block++ {
		n, err := src.ReadAt(buf, block*blockSize)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if err := os.WriteFile(c.path(block), buf[:n], 0600); err != nil {
			return err
		}
	}
	return nil
}

func (c *DirCache) path(block int64) string {
	return filepath.Join(c.dir, fmt.Sprintf(blockName, block))
}
