package source

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
)

const (
	missingMember = "missing"
)

var (
	resourceSuffix = regexp.MustCompile(`#r[0-9a-f]{16}`) // what HTTP.Identity adds since v0.4.2
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

// LegacyIdentity returns identity as releases before v0.4.2 computed it: without the
// resource HTTP sources name. An overlay recorded with that form is accepted and re-pinned
// (see cow.Options.LegacyIdentity), so an upgrade needs no blkmap pin.
func LegacyIdentity(identity string) string {
	return resourceSuffix.ReplaceAllString(identity, "")
}

func (f *File) Identity() string {
	return f.ident
}

func (h *HTTP) Identity() string {
	version := "etag=" + h.etag
	if h.etag == "" {
		version = "modified=" + h.modified
	}
	// Validators are unique only per resource (RFC 9110 8.8.1): another URL with the same
	// ETag and size may hold other bytes, so the resource is part of the identity, hashed to
	// keep it short and free of characters the composite forms use
	r := fnv.New64a()
	r.Write([]byte(h.resource))
	return fmt.Sprintf("http:%d:%s@%d+%d#r%016x", h.total, version, h.offset, h.size, r.Sum64())
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

// Identity is the geometry plus the ordered members: swapping two members changes the
// content with the same geometry. A degraded array therefore has its own identity, and an
// overlay made over the full array needs `blkmap pin` to be accepted over it.
func (r *RAID5) Identity() string {
	parts := make([]string, len(r.members))
	for i, m := range r.members {
		parts[i] = missingMember
		if m != nil {
			parts[i] = Identity(m)
		}
	}
	return fmt.Sprintf("raid5:%d:%d:%d:%d(%s)", len(r.members), r.stripeSize, r.layout, r.size, strings.Join(parts, ";"))
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
