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
	// httpMaxConns bounds the connections one http source opens to its origin, so parallel
	// dispatch plus read-ahead cannot pile up hundreds of sockets against a slow server.
	httpMaxConns = 16
	// mapSuffix is probed next to an http image when no map is configured.
	mapSuffix = ".map"
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

// Sparse is implemented by sources that can tell which parts of a range are holes (read
// as zeros), so a hydrator can mark them without copying anything. Files and devices
// answer from SEEK_HOLE; containers compose their parts; a custom source can answer from
// whatever map it has. A hole must read as zeros; data regions may contain zeros too.
type Sparse interface {
	Holes(off, length int64) ([]Range, error)
}

// ReadDirect reads bypassing caches where the source supports it, else like ReadAt.
func ReadDirect(s Source, p []byte, off int64) (int, error) {
	if d, ok := s.(DirectReader); ok {
		return d.ReadAtDirect(p, off)
	}
	return s.ReadAt(p, off)
}

// Aborter is implemented by sources whose reads can block on something external (a
// network); Abort makes in-flight and later reads fail at once so a stopping device never
// waits on them. Containers forward it to their parts.
type Aborter interface {
	Abort()
}

// Present is implemented by sources that know which parts of them hold data: a sparse file
// (SEEK_HOLE), a mapped source. A cache's fast tier that implements it answers a miss for
// any range not fully present, so a partial local copy serves what it has and never zeros
// for what it lacks. Must not allocate: it runs on every cached read.
type Present interface {
	Present(off, length int64) bool
}

// Abort aborts s if it can be aborted.
func Abort(s Source) {
	if a, ok := s.(Aborter); ok {
		a.Abort()
	}
}

// Holes returns the holes of s within [off, off+length), ascending and non-overlapping,
// or nil when the source cannot tell.
func Holes(s Source, off, length int64) ([]Range, error) {
	if sp, ok := s.(Sparse); ok {
		return sp.Holes(off, length)
	}
	return nil, nil
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

// open creates the Source for one config segment, attaching its map if it has one.
func open(s *config.Segment) (Source, error) {
	src, err := openPlain(s)
	if err != nil {
		return nil, err
	}
	m, err := mapFor(s)
	if err != nil {
		src.Close()
		return nil, err
	}
	if m == nil {
		return src, nil
	}
	return WithMap(src, m, s.SourceOffset), nil
}

// mapFor loads the segment's configured map, or probes <url>.map for an http source.
func mapFor(s *config.Segment) (*Map, error) {
	if s.Map != "" {
		m, err := LoadMap(s.Map)
		if err != nil {
			return nil, fmt.Errorf("map: %w", err)
		}
		return m, nil
	}
	if s.Type != config.SourceHTTP {
		return nil, nil
	}
	m, err := LoadMap(s.URL + mapSuffix)
	if err != nil {
		return nil, nil // no sidecar: the source is opaque about holes
	}
	return m, nil
}

// openPlain creates the Source for one config segment without its map.
func openPlain(s *config.Segment) (Source, error) {
	switch s.Type {
	case config.SourceZero:
		return NewZero(s.Size), nil
	case config.SourceFile, config.SourceDevice:
		return OpenFile(s.Path, s.SourceOffset, s.Size)
	case config.SourceHTTP:
		return NewHTTP(newHTTPClient(), s.URL, s.SourceOffset, s.Size)
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

// newHTTPClient returns the client an http segment uses: a transport of its own with a
// bounded connection count, and a timeout per fetch.
func newHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxConnsPerHost = httpMaxConns
	transport.MaxIdleConnsPerHost = httpMaxConns
	return &http.Client{Timeout: httpTimeout, Transport: transport}
}

// Walk calls fn for s and every source it is built from.
func Walk(s Source, fn func(Source)) {
	fn(s)
	if c, ok := s.(composite); ok {
		for _, part := range c.parts() {
			Walk(part, fn)
		}
	}
}

// composite is implemented by sources built from other sources.
type composite interface {
	parts() []Source
}
