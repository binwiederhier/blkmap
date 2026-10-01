package source

import (
	"errors"
	"fmt"
)

// Concat stitches ordered, non-overlapping segments into one address space. Gaps between
// segments and the tail up to size read as zeros.
type Concat struct {
	segments []*Segment
	size     int64
}

// NewConcat validates the layout and returns the stitched source. A size of 0 means the end
// of the last segment.
func NewConcat(segments []*Segment, size int64) (*Concat, error) {
	if len(segments) == 0 {
		return nil, fmt.Errorf("at least one segment is required")
	}
	var end int64
	for i, s := range segments {
		if s.Source.Size() <= 0 {
			return nil, fmt.Errorf("segment %d is empty", i)
		}
		if s.Offset < end {
			return nil, fmt.Errorf("segment %d at offset %d overlaps the previous segment ending at %d", i, s.Offset, end)
		}
		end = s.Offset + s.Source.Size()
	}
	if size == 0 {
		size = end
	}
	if end > size {
		return nil, fmt.Errorf("segments end at %d, which exceeds the device size %d", end, size)
	}
	return &Concat{segments: segments, size: size}, nil
}

// Segments returns the resolved layout in device order.
func (c *Concat) Segments() []*Segment {
	return c.segments
}

func (c *Concat) ReadAt(p []byte, off int64) (int, error) {
	return c.readAt(p, off, false)
}

// ReadAtDirect reads every segment around its cache tier, if it has one.
func (c *Concat) ReadAtDirect(p []byte, off int64) (int, error) {
	return c.readAt(p, off, true)
}

// ZeroRanges reports the gaps, the tail, and whatever the segments report, in order.
func (c *Concat) ZeroRanges() []Range {
	var ranges []Range
	var pos int64
	for _, s := range c.segments {
		if s.Offset > pos {
			ranges = append(ranges, Range{Offset: pos, Length: s.Offset - pos})
		}
		for _, r := range ZeroRanges(s.Source) {
			ranges = append(ranges, Range{Offset: s.Offset + r.Offset, Length: r.Length})
		}
		pos = s.Offset + s.Source.Size()
	}
	if pos < c.size {
		ranges = append(ranges, Range{Offset: pos, Length: c.size - pos})
	}
	return ranges
}

func (c *Concat) readAt(p []byte, off int64, direct bool) (int, error) {
	n, eof := clampRead(len(p), off, c.size)
	p = p[:n]
	for len(p) > 0 {
		s, next := c.locate(off)
		var m int
		if s == nil {
			// In a gap (or the tail): zeros up to the next segment or the end of the buffer
			m = int(min(int64(len(p)), next-off))
			clear(p[:m])
		} else {
			m = int(min(int64(len(p)), s.Offset+s.Source.Size()-off))
			var read int
			var err error
			if direct {
				read, err = ReadDirect(s.Source, p[:m], off-s.Offset)
			} else {
				read, err = s.Source.ReadAt(p[:m], off-s.Offset)
			}
			if read < m {
				if err == nil {
					err = fmt.Errorf("short read")
				}
				return n - len(p) + read, fmt.Errorf("segment at offset %d: %w", s.Offset, err)
			}
		}
		p = p[m:]
		off += int64(m)
	}
	return n, eof
}

func (c *Concat) Size() int64 {
	return c.size
}

// Close closes every segment source and returns the first error.
func (c *Concat) Close() error {
	var errs []error
	for _, s := range c.segments {
		errs = append(errs, s.Source.Close())
	}
	return errors.Join(errs...)
}

// locate finds the segment containing off, or nil and the offset where the next segment (or
// the device) ends if off lies in a gap.
func (c *Concat) locate(off int64) (*Segment, int64) {
	for _, s := range c.segments {
		if off < s.Offset {
			return nil, s.Offset
		}
		if off < s.Offset+s.Source.Size() {
			return s, 0
		}
	}
	return nil, c.size
}
