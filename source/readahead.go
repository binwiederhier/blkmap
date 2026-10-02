package source

import (
	"errors"
	"io"
)

// ReadAhead wraps a source with the block cache: 1 MiB blocks, single-flight fetches, and
// sequential read-ahead. It is for sources whose reads are expensive round trips (a remote
// protocol, a custom source over a network) and that have no cache of their own; the http
// source has it built in, and local files get the kernel's page cache.
type ReadAhead struct {
	src   Source
	cache *blockCache
}

func NewReadAhead(src Source) *ReadAhead {
	r := &ReadAhead{src: src}
	r.cache = newBlockCache(src.Size(), r.fetchBlock)
	if _, sparse := src.(Sparse); sparse {
		r.cache.skip = r.isHole
	}
	return r
}

// setMap lets the read-ahead skip blocks the map says are holes.
func (r *ReadAhead) setMap(m *Map) {
	r.cache.skip = func(index int64) bool {
		start := index * cacheBlockSize
		return !m.HasData(start, min(cacheBlockSize, r.src.Size()-start))
	}
}

// Abort aborts the inner source.
func (r *ReadAhead) Abort() {
	Abort(r.src)
}

// isHole reports whether a whole block lies in a hole of the inner source.
func (r *ReadAhead) isHole(index int64) bool {
	start := index * cacheBlockSize
	length := min(cacheBlockSize, r.src.Size()-start)
	holes, err := Holes(r.src, start, length)
	return err == nil && len(holes) == 1 && holes[0].Offset == start && holes[0].Length == length
}

func (r *ReadAhead) ReadAt(p []byte, off int64) (int, error) {
	n, eof := clampRead(len(p), off, r.src.Size())
	if err := r.cache.readAt(p[:n], off); err != nil {
		return 0, err
	}
	return n, eof
}

// ReadAtDirect bypasses the cache (and does not feed it), for hydration with use-cache never.
func (r *ReadAhead) ReadAtDirect(p []byte, off int64) (int, error) {
	return ReadDirect(r.src, p, off)
}

func (r *ReadAhead) Holes(off, length int64) ([]Range, error) {
	return Holes(r.src, off, length)
}

func (r *ReadAhead) Size() int64 {
	return r.src.Size()
}

func (r *ReadAhead) Close() error {
	return r.src.Close()
}

// fetchBlock reads one whole block (the last one may be short) from the inner source.
func (r *ReadAhead) fetchBlock(index int64) ([]byte, error) {
	start := index * cacheBlockSize
	data := make([]byte, min(cacheBlockSize, r.src.Size()-start))
	n, err := r.src.ReadAt(data, start)
	if err != nil && !(errors.Is(err, io.EOF) && n == len(data)) {
		return nil, err
	}
	if n < len(data) {
		return nil, io.ErrUnexpectedEOF
	}
	return data, nil
}
