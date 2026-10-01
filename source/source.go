// Package source provides the read-only byte ranges a blkmap device is stitched from.
package source

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"heckel.io/blkmap/config"
)

const (
	// httpTimeout bounds one block fetch; blocks are 1 MiB so this is generous.
	httpTimeout = 60 * time.Second
)

// Source is a fixed-size, read-only byte range. ReadAt follows io.ReaderAt semantics; a read
// past Size returns a short count and io.EOF.
type Source interface {
	io.ReaderAt
	Size() int64
	Close() error
}

// Segment places a Source at an offset of the device address space.
type Segment struct {
	Offset int64
	Source Source
}

// FromConfig opens every configured segment, resolves implicit offsets and sizes, and
// returns the stitched device-sized Concat.
func FromConfig(c *config.Config) (*Concat, error) {
	var segments []*Segment
	var end int64
	closeAll := func() {
		for _, s := range segments {
			s.Source.Close()
		}
	}
	for i, cs := range c.Segments {
		src, err := open(cs)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("segment %d: %w", i, err)
		}
		offset := cs.Offset
		if offset < 0 {
			offset = end
		}
		segments = append(segments, &Segment{Offset: offset, Source: src})
		end = offset + src.Size()
	}
	concat, err := NewConcat(segments, c.Size)
	if err != nil {
		closeAll()
		return nil, err
	}
	return concat, nil
}

// open creates the Source for one config segment.
func open(s *config.Segment) (Source, error) {
	switch s.Type {
	case config.SourceZero:
		return NewZero(s.Size), nil
	case config.SourceFile, config.SourceDevice:
		return OpenFile(s.Path, s.SourceOffset, s.Size)
	case config.SourceHTTP:
		return NewHTTP(&http.Client{Timeout: httpTimeout}, s.URL, s.SourceOffset, s.Size)
	default:
		return nil, fmt.Errorf("unknown segment type %q", s.Type)
	}
}
