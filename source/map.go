package source

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
)

const (
	mapURLPrefixHTTP  = "http://"
	mapURLPrefixHTTPS = "https://"
	// maxMapBytes bounds a map fetched over http; maxRanges lines fit comfortably.
	maxMapBytes = 64 << 20
)

var (
	errMapTooLarge = fmt.Errorf("map larger than %d bytes", maxMapBytes)
)

// Map is the data layout of a source: sorted, merged data extents; everything else reads as
// zeros. It is how a source that cannot tell holes itself (an HTTP image, a custom source
// fed by something that knows) gets them, so hydration never transfers zeros.
type Map struct {
	extents []Range
}

// NewMap builds a map from data extents in any order; overlapping and adjacent extents
// are merged. Negative or empty extents are an error.
func NewMap(data []Range) (*Map, error) {
	extents := make([]Range, 0, len(data))
	for _, r := range data {
		if r.Offset < 0 || r.Length <= 0 || r.Offset > math.MaxInt64-r.Length {
			return nil, fmt.Errorf("invalid map extent: offset %d, length %d", r.Offset, r.Length)
		}
		extents = append(extents, r)
	}
	sort.Slice(extents, func(i, j int) bool { return extents[i].Offset < extents[j].Offset })
	merged := extents[:0]
	for _, r := range extents {
		if n := len(merged); n > 0 && r.Offset <= merged[n-1].Offset+merged[n-1].Length {
			merged[n-1].Length = max(merged[n-1].Length, r.Offset+r.Length-merged[n-1].Offset)
			continue
		}
		merged = append(merged, r)
	}
	return &Map{extents: merged}, nil
}

// ParseMap reads a map in the prefetch-list format: one "offset length" data extent per
// line, binary suffixes, '#' comments.
func ParseMap(r io.Reader) (*Map, error) {
	ranges, err := parseRanges(r, "map")
	if err != nil {
		return nil, err
	}
	return NewMap(ranges)
}

// LoadMap reads a map from a file path or an http(s) URL.
func LoadMap(pathOrURL string) (*Map, error) {
	var r io.ReadCloser
	name := pathOrURL
	if strings.HasPrefix(pathOrURL, mapURLPrefixHTTP) || strings.HasPrefix(pathOrURL, mapURLPrefixHTTPS) {
		name = safeURL(pathOrURL) // never credentials or tokens in an error
		resp, err := (&http.Client{Timeout: httpTimeout}).Get(pathOrURL)
		if err != nil {
			return nil, safeErr(err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("%s: got %d %s", name, resp.StatusCode, http.StatusText(resp.StatusCode))
		}
		// A body over the limit is an error, not an early end: a truncated map would drop
		// extents, which then read as zeros
		r = struct {
			io.Reader
			io.Closer
		}{&mapLimit{r: resp.Body, left: maxMapBytes}, resp.Body}
	} else {
		f, err := os.Open(pathOrURL)
		if err != nil {
			return nil, err
		}
		r = f
	}
	defer r.Close()
	m, err := ParseMap(r)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return m, nil
}

// mapLimit reads at most left bytes and fails, rather than ending, on any byte beyond them.
type mapLimit struct {
	r    io.Reader
	left int64
}

func (l *mapLimit) Read(p []byte) (int, error) {
	if l.left < 0 {
		return 0, errMapTooLarge
	}
	if int64(len(p)) > l.left+1 {
		p = p[:l.left+1] // one byte past the limit tells a larger body from one that fits
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	if l.left < 0 {
		return n + int(l.left), errMapTooLarge
	}
	return n, err
}

// Data returns the data extents within [off, off+length), clipped, ascending.
func (m *Map) Data(off, length int64) []Range {
	end := off + length
	var out []Range
	for i := m.first(off); i < len(m.extents) && m.extents[i].Offset < end; i++ {
		e := m.extents[i]
		start, stop := max(e.Offset, off), min(e.Offset+e.Length, end)
		out = append(out, Range{Offset: start, Length: stop - start})
	}
	return out
}

// HasData reports whether any data extent intersects [off, off+length).
func (m *Map) HasData(off, length int64) bool {
	i := m.first(off)
	return i < len(m.extents) && m.extents[i].Offset-off < length
}

// Covers reports whether [off, off+length) lies entirely within one data extent (extents
// are merged, so touching extents are one).
func (m *Map) Covers(off, length int64) bool {
	i := m.first(off)
	if i >= len(m.extents) || length <= 0 {
		return false
	}
	e := m.extents[i]
	return e.Offset <= off && off+length <= e.Offset+e.Length
}

// Holes returns the holes within [off, off+length), clipped, ascending.
func (m *Map) Holes(off, length int64) []Range {
	end := off + length
	var out []Range
	pos := off
	for i := m.first(off); i < len(m.extents) && m.extents[i].Offset < end; i++ {
		e := m.extents[i]
		if e.Offset > pos {
			out = append(out, Range{Offset: pos, Length: e.Offset - pos})
		}
		pos = max(pos, e.Offset+e.Length)
	}
	if pos < end {
		out = append(out, Range{Offset: pos, Length: end - pos})
	}
	return out
}

// Extents returns every data extent.
func (m *Map) Extents() []Range {
	return m.extents
}

// DataBytes returns the total bytes of data.
func (m *Map) DataBytes() int64 {
	var n int64
	for _, e := range m.extents {
		n += e.Length
	}
	return n
}

// first returns the index of the first extent ending after pos.
func (m *Map) first(pos int64) int {
	return sort.Search(len(m.extents), func(i int) bool { return m.extents[i].Offset+m.extents[i].Length > pos })
}

// Mapped is a source with an attached Map: holes read as zeros without touching the inner
// source, and Holes answers from the map. The map's coordinates are those of the inner
// source's underlying resource; base is where the inner source's offset 0 sits in them
// (a file opened at source-offset 1M has base 1M).
type Mapped struct {
	src  Source
	m    *Map
	base int64
}

// mapAware is implemented by sources with a read-ahead cache that can skip hole blocks.
type mapAware interface {
	setMap(m *Map)
}

func WithMap(src Source, m *Map, base int64) *Mapped {
	if aware, ok := src.(mapAware); ok && base == 0 {
		aware.setMap(m)
	}
	return &Mapped{src: src, m: m, base: base}
}

func (d *Mapped) ReadAt(p []byte, off int64) (int, error) {
	return d.readAt(p, off, false)
}

func (d *Mapped) ReadAtDirect(p []byte, off int64) (int, error) {
	return d.readAt(p, off, true)
}

// readAt zero-fills the holes locally and reads only the data extents from the inner source.
func (d *Mapped) readAt(p []byte, off int64, direct bool) (int, error) {
	n, eof := clampRead(len(p), off, d.src.Size())
	p = p[:n]
	pos := d.base + off // in the map's (resource) coordinates
	end := pos + int64(n)
	for i := d.m.first(pos); i < len(d.m.extents) && d.m.extents[i].Offset < end; i++ {
		e := d.m.extents[i]
		if e.Offset > pos { // hole before this extent
			clear(p[:e.Offset-pos])
			p = p[e.Offset-pos:]
			pos = e.Offset
		}
		m := int(min(e.Offset+e.Length, end) - pos)
		var read int
		var err error
		if direct {
			read, err = ReadDirect(d.src, p[:m], pos-d.base)
		} else {
			read, err = d.src.ReadAt(p[:m], pos-d.base)
		}
		if err != nil && !(errors.Is(err, io.EOF) && read == m) {
			return n - len(p) + read, err
		}
		if read < m {
			return n - len(p) + read, io.ErrUnexpectedEOF
		}
		p = p[m:]
		pos += int64(m)
	}
	clear(p) // trailing hole
	return n, eof
}

func (d *Mapped) Holes(off, length int64) ([]Range, error) {
	length = min(length, d.src.Size()-off)
	if off < 0 || length <= 0 {
		return nil, nil
	}
	holes := d.m.Holes(d.base+off, length)
	for i := range holes {
		holes[i].Offset -= d.base
	}
	return holes, nil
}

// Present reports whether the range lies within the map's data extents.
func (d *Mapped) Present(off, length int64) bool {
	return d.m.Covers(d.base+off, length)
}

// Abort aborts the inner source.
func (d *Mapped) Abort() {
	Abort(d.src)
}

// Map returns the attached map.
func (d *Mapped) Map() *Map {
	return d.m
}

func (d *Mapped) Size() int64 {
	return d.src.Size()
}

func (d *Mapped) Close() error {
	return d.src.Close()
}

// Extents returns the data extents of any source, as a map would list them: the complement
// of Holes. A source that cannot tell is one data extent.
func Extents(s Source) ([]Range, error) {
	size := s.Size()
	holes, err := Holes(s, 0, size)
	if err != nil {
		return nil, err
	}
	if holes == nil {
		return []Range{{Offset: 0, Length: size}}, nil
	}
	var data []Range
	var pos int64
	for _, h := range holes {
		if h.Offset > pos {
			data = append(data, Range{Offset: pos, Length: h.Offset - pos})
		}
		pos = h.Offset + h.Length
	}
	if pos < size {
		data = append(data, Range{Offset: pos, Length: size - pos})
	}
	return data, nil
}
