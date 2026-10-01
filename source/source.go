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

var (
	layoutNames = map[string]Layout{
		config.LayoutLeftSymmetric:   LeftSymmetric,
		config.LayoutLeftAsymmetric:  LeftAsymmetric,
		config.LayoutRightSymmetric:  RightSymmetric,
		config.LayoutRightAsymmetric: RightAsymmetric,
	}
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

// DirectReader is implemented by sources that can read around a cache tier (Cache itself,
// and containers that forward to their parts).
type DirectReader interface {
	ReadAtDirect(p []byte, off int64) (int, error)
}

// Zeroer is implemented by sources that know which of their ranges read as zeros, so a
// hydrator can mark them without copying anything.
type Zeroer interface {
	ZeroRanges() []Range
}

// ReadDirect reads bypassing caches where the source supports it, else like ReadAt.
func ReadDirect(s Source, p []byte, off int64) (int, error) {
	if d, ok := s.(DirectReader); ok {
		return d.ReadAtDirect(p, off)
	}
	return s.ReadAt(p, off)
}

// ZeroRanges returns the ranges of s known to read as zeros, in ascending order.
func ZeroRanges(s Source) []Range {
	if z, ok := s.(Zeroer); ok {
		return z.ZeroRanges()
	}
	return nil
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
	case config.SourceRAID5:
		return openRAID5(s)
	case config.SourceCache:
		fast, err := open(s.Fast)
		if err != nil {
			return nil, fmt.Errorf("fast: %w", err)
		}
		slow, err := open(s.Slow)
		if err != nil {
			fast.Close()
			return nil, fmt.Errorf("slow: %w", err)
		}
		return NewCache(fast, slow), nil
	case config.SourceCustom:
		return NewCustom(s.Name, s.Size, s.Params)
	default:
		return nil, fmt.Errorf("unknown segment type %q", s.Type)
	}
}

// openRAID5 opens every present member of a raid5 segment and assembles the array.
func openRAID5(s *config.Segment) (Source, error) {
	members := make([]Source, len(s.Members))
	closeAll := func() {
		for _, m := range members {
			if m != nil {
				m.Close()
			}
		}
	}
	for i, m := range s.Members {
		if m.Missing {
			continue
		}
		src, err := open(m)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("member %d: %w", i, err)
		}
		members[i] = src
	}
	layout, ok := layoutNames[s.Layout]
	if !ok {
		closeAll()
		return nil, fmt.Errorf("unknown raid5 layout %q", s.Layout)
	}
	r, err := NewRAID5(members, s.StripeSize, layout, s.Size)
	if err != nil {
		closeAll()
		return nil, err
	}
	return r, nil
}
