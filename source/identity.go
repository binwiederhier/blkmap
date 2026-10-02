package source

import (
	"fmt"
	"hash/fnv"
	"strings"
)

// Identifier is implemented by sources that can fingerprint the version of their content:
// the same string means the same bytes. The COW store records it and refuses a base whose
// fingerprint changed, since overlaying old writes on new content corrupts the device.
type Identifier interface {
	Identity() string
}

// Identity returns the fingerprint of s's content. A source that cannot tell its version
// is identified by its type and size alone, which still catches a resized source.
func Identity(s Source) string {
	if i, ok := s.(Identifier); ok {
		return i.Identity()
	}
	return fmt.Sprintf("%T:%d", s, s.Size())
}

func (f *File) Identity() string {
	return f.ident
}

func (h *HTTP) Identity() string {
	version := "etag=" + h.etag
	if h.etag == "" {
		version = "modified=" + h.modified
	}
	return fmt.Sprintf("http:%d:%s@%d+%d", h.total, version, h.offset, h.size)
}

func (z *Zero) Identity() string {
	return fmt.Sprintf("zero:%d", z.size)
}

func (c *Concat) Identity() string {
	parts := make([]string, len(c.segments))
	for i, s := range c.segments {
		parts[i] = fmt.Sprintf("%d:%s", s.Offset, Identity(s.Source))
	}
	return fmt.Sprintf("concat:%d(%s)", c.size, strings.Join(parts, ";"))
}

// Identity is the slow source's: the fast tier is a copy managed elsewhere.
func (c *Cache) Identity() string {
	return Identity(c.slow)
}

// Identity is the array's geometry: which members are present does not change the content,
// and members are usually devices, which cannot tell their version anyway.
func (r *RAID5) Identity() string {
	return fmt.Sprintf("raid5:%d:%d:%d:%d", len(r.members), r.stripeSize, r.layout, r.size)
}

func (d *Mapped) Identity() string {
	h := fnv.New64a()
	for _, e := range d.m.extents {
		fmt.Fprintf(h, "%d+%d,", e.Offset, e.Length)
	}
	return fmt.Sprintf("%s+map:%d:%x", Identity(d.src), d.base, h.Sum64())
}

func (r *ReadAhead) Identity() string {
	return Identity(r.src)
}

func (s *Swappable) Identity() string {
	return Identity(*s.current.Load())
}

func (c *Concat) parts() []Source {
	parts := make([]Source, len(c.segments))
	for i, s := range c.segments {
		parts[i] = s.Source
	}
	return parts
}

func (c *Cache) parts() []Source {
	return []Source{c.fast, c.slow}
}

func (r *RAID5) parts() []Source {
	var parts []Source
	for _, m := range r.members {
		if m != nil {
			parts = append(parts, m)
		}
	}
	return parts
}

func (d *Mapped) parts() []Source {
	return []Source{d.src}
}

func (r *ReadAhead) parts() []Source {
	return []Source{r.src}
}

func (s *Swappable) parts() []Source {
	return []Source{*s.current.Load()}
}
