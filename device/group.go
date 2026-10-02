package device

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"heckel.io/blkmap/cow"
	"heckel.io/blkmap/source"
)

// Alias declares that [Offset, Offset+Length) of a device is a view of
// [TargetOffset, TargetOffset+Length) of another device in the same group: reads and writes
// there go to the target's store, so both devices always show the same bytes and a write
// through either is stored once. A mirror's second plex is an alias of its first.
type Alias struct {
	Offset       int64
	Length       int64
	Target       string // device id within the group
	TargetOffset int64
}

// GroupOptions describes one device of a group: Options plus its aliases into siblings.
type GroupOptions struct {
	Options
	Aliases []Alias
	// ElideIdenticalWrites drops writes whose bytes equal what the device already reads
	// there (see cow.Store.SetElision).
	ElideIdenticalWrites bool
}

// Group is a set of devices served by one process whose bases may read each other through
// their live views and whose ranges may alias each other.
type Group struct {
	Devices map[string]*Device
	order   []string
}

// ServeGroup opens every device's store, binds the sources that read siblings, then brings
// the kernel devices up. Any failure closes what was opened. ServeGroup owns every Base.
func ServeGroup(ctx context.Context, opts []*GroupOptions) (*Group, error) {
	if len(opts) == 0 {
		return nil, errors.New("a group needs at least one device")
	}
	if err := ctx.Err(); err != nil {
		for _, o := range opts {
			if o.Base != nil {
				o.Base.Close()
			}
		}
		return nil, err
	}
	routers := make(map[string]*router, len(opts))
	closeStores := func() {
		for id, r := range routers {
			closeStore(id, r.store, r.pred)
		}
	}
	for _, o := range opts {
		if _, dup := routers[o.ID]; dup {
			closeStores()
			return nil, fmt.Errorf("duplicate device id %q", o.ID)
		}
		if o.Hydrate != nil && len(o.Aliases) > 0 {
			closeStores()
			return nil, fmt.Errorf("%s: hydration and aliases cannot be combined", o.ID)
		}
		store, pred, err := openStore(&o.Options)
		if err != nil {
			closeStores()
			return nil, fmt.Errorf("%s: %w", o.ID, err)
		}
		store.SetElision(o.ElideIdenticalWrites)
		routers[o.ID] = &router{id: o.ID, store: store, pred: pred}
	}
	if err := setAllAliases(opts, routers); err != nil {
		closeStores()
		return nil, err
	}
	bases := make([]source.Source, len(opts))
	for i, o := range opts {
		bases[i] = o.Base
	}
	bindAll(bases, func(id string) (io.ReaderAt, bool) {
		r, ok := routers[id]
		return r, ok
	})
	g := &Group{Devices: make(map[string]*Device, len(opts))}
	for _, o := range opts {
		d, err := serveStore(ctx, &o.Options, routers[o.ID].store, routers[o.ID].pred, routers[o.ID])
		if err != nil {
			g.Close()
			for id, r := range routers { // stores of devices that never came up
				if _, up := g.Devices[id]; !up {
					closeStore(id, r.store, r.pred)
				}
			}
			return nil, fmt.Errorf("%s: %w", o.ID, err)
		}
		g.Devices[o.ID] = d
		g.order = append(g.order, o.ID)
	}
	return g, nil
}

// Close shuts the devices down: first every device's I/O ends (an aliased request or a derived
// base reads a sibling's store, so no store may close while any device still serves), then
// the devices close in reverse order of creation.
func (g *Group) Close() error {
	var errs []error
	for _, id := range g.order {
		errs = append(errs, g.Devices[id].halt())
	}
	for i := len(g.order) - 1; i >= 0; i-- {
		if d := g.Devices[g.order[i]]; d != nil {
			errs = append(errs, d.Close())
		}
	}
	return errors.Join(errs...)
}

// router is the I/O entry point of one group device: ranges covered by an alias go to the
// target device's router, everything else to the device's own store.
type router struct {
	id      string
	store   *cow.Store
	pred    *predecessor // the kernel device a previous server left for recovery, until serveStore takes it
	aliases []alias
	targets []*router // every distinct alias target, for Flush
}

type alias struct {
	Alias
	target *router
}

func (r *router) setAliases(aliases []Alias, routers map[string]*router) error {
	size := r.store.Size()
	sorted := make([]alias, 0, len(aliases))
	seen := map[*router]bool{}
	for _, a := range aliases {
		if a.Length <= 0 || a.Offset < 0 || a.Offset+a.Length > size {
			return fmt.Errorf("alias [%d, %d) is outside the device (size %d)", a.Offset, a.Offset+a.Length, size)
		}
		t, ok := routers[a.Target]
		if !ok {
			return fmt.Errorf("alias target %q is not in the group", a.Target)
		}
		if t == r {
			return errors.New("a device cannot alias itself")
		}
		if a.TargetOffset < 0 || a.TargetOffset+a.Length > t.store.Size() {
			return fmt.Errorf("alias target range [%d, %d) is outside %s (size %d)", a.TargetOffset, a.TargetOffset+a.Length, a.Target, t.store.Size())
		}
		sorted = append(sorted, alias{Alias: a, target: t})
		if !seen[t] {
			seen[t] = true
			r.targets = append(r.targets, t)
		}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Offset < sorted[j].Offset })
	for i := 1; i < len(sorted); i++ {
		if sorted[i].Offset < sorted[i-1].Offset+sorted[i-1].Length {
			return fmt.Errorf("aliases [%d, %d) and [%d, %d) overlap", sorted[i-1].Offset, sorted[i-1].Offset+sorted[i-1].Length, sorted[i].Offset, sorted[i].Offset+sorted[i].Length)
		}
	}
	r.aliases = sorted
	return nil
}

// piece is one part of a request: either in an alias (target set) or in the own store.
type piece struct {
	off, length int64 // in the request's device
	target      *router
	targetOff   int64
}

// pieces walks [off, off+length) in pieces cut at alias boundaries, without allocating: a
// group device sends every guest request through it.
type pieces struct {
	r        *router
	off, end int64
	i        int // index of the next alias that can intersect
}

func (r *router) pieces(off, length int64) pieces {
	i := sort.Search(len(r.aliases), func(i int) bool { return r.aliases[i].Offset+r.aliases[i].Length > off })
	return pieces{r: r, off: off, end: off + length, i: i}
}

// next returns the next piece; ok is false when the range is exhausted.
func (it *pieces) next() (pc piece, ok bool) {
	if it.off >= it.end {
		return piece{}, false
	}
	aliases := it.r.aliases
	if it.i >= len(aliases) || aliases[it.i].Offset >= it.end {
		pc = piece{off: it.off, length: it.end - it.off}
	} else if a := aliases[it.i]; a.Offset > it.off {
		pc = piece{off: it.off, length: a.Offset - it.off}
	} else {
		pc = piece{off: it.off, length: min(it.end, a.Offset+a.Length) - it.off, target: a.target, targetOff: a.TargetOffset + (it.off - a.Offset)}
		it.i++
	}
	it.off += pc.length
	return pc, true
}

func (r *router) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	for it := r.pieces(off, int64(len(p))); ; {
		pc, ok := it.next()
		if !ok {
			return n, nil
		}
		buf := p[pc.off-off : pc.off-off+pc.length]
		var m int
		var err error
		if pc.target != nil {
			m, err = pc.target.ReadAt(buf, pc.targetOff)
		} else {
			m, err = r.store.ReadAt(buf, pc.off)
		}
		n += m
		if err != nil && !(errors.Is(err, io.EOF) && m == len(buf)) {
			return n, err
		}
	}
}

func (r *router) WriteAt(p []byte, off int64) (int, error) {
	n := 0
	for it := r.pieces(off, int64(len(p))); ; {
		pc, ok := it.next()
		if !ok {
			return n, nil
		}
		buf := p[pc.off-off : pc.off-off+pc.length]
		var m int
		var err error
		if pc.target != nil {
			m, err = pc.target.WriteAt(buf, pc.targetOff)
		} else {
			m, err = r.store.WriteAt(buf, pc.off)
		}
		n += m
		if err != nil {
			return n, err
		}
	}
}

func (r *router) Size() int64 { return r.store.Size() }

// Flush makes the own store and every alias target durable.
func (r *router) Flush() error {
	errs := []error{r.store.Flush()}
	for _, t := range r.targets {
		errs = append(errs, t.Flush())
	}
	return errors.Join(errs...)
}

func (r *router) Discard(off, length int64) error {
	for it := r.pieces(off, length); ; {
		pc, ok := it.next()
		if !ok {
			return nil
		}
		var err error
		if pc.target != nil {
			err = pc.target.Discard(pc.targetOff, pc.length)
		} else {
			err = r.store.Discard(pc.off, pc.length)
		}
		if err != nil {
			return err
		}
	}
}

func (r *router) WriteZeroes(off, length int64) error {
	for it := r.pieces(off, length); ; {
		pc, ok := it.next()
		if !ok {
			return nil
		}
		var err error
		if pc.target != nil {
			err = pc.target.WriteZeroes(pc.targetOff, pc.length)
		} else {
			err = r.store.WriteZeroes(pc.off, pc.length)
		}
		if err != nil {
			return err
		}
	}
}

// setAllAliases validates and installs every device's aliases. An alias must land in a range
// of its target that the target serves itself: following aliases further (a chain, or two
// devices aliasing each other) is refused rather than forwarded.
func setAllAliases(opts []*GroupOptions, routers map[string]*router) error {
	for _, o := range opts {
		if err := routers[o.ID].setAliases(o.Aliases, routers); err != nil {
			return fmt.Errorf("%s: %w", o.ID, err)
		}
	}
	for _, o := range opts {
		for _, a := range routers[o.ID].aliases {
			for _, t := range a.target.aliases {
				if a.TargetOffset < t.Offset+t.Length && t.Offset < a.TargetOffset+a.Length {
					return fmt.Errorf("%s: alias [%d, %d) lands in %s's own alias [%d, %d); aliases cannot be chained", o.ID, a.Offset, a.Offset+a.Length, a.Target, t.Offset, t.Offset+t.Length)
				}
			}
		}
	}
	return nil
}

// bindAll hands lookup to every Binder among the bases, however deep in a base's tree (a
// config always stitches its segments into a Concat).
func bindAll(bases []source.Source, lookup source.Lookup) {
	for _, base := range bases {
		source.Walk(base, func(s source.Source) {
			if b, ok := s.(source.Binder); ok {
				b.Bind(lookup)
			}
		})
	}
}
