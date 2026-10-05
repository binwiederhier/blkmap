package device

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"heckel.io/blkmap/cow"
	"heckel.io/blkmap/source"
)

// Group is a set of devices served by one process whose bases may read each other through
// their live views: a base that implements source.Binder receives a source.Lookup of its
// siblings before any device serves I/O, so a RAID member's parity can be the XOR of the
// data members as the guest sees them, and a mirror's second plex a view of the first.
// Every device keeps its own COW overlay: a write to one device reaches another only by
// being read through such a view, never by landing in its store.
type Group struct {
	Devices   map[string]*Device
	order     []string
	failed    chan struct{} // see Done
	closing   chan struct{} // closed by Close and Abandon: ends watch
	failOnce  sync.Once
	closeOnce sync.Once
}

// ServeGroup opens every device's store, binds the sources that read siblings, then brings
// the kernel devices up. Any failure closes what was opened. ServeGroup owns every Base.
func ServeGroup(ctx context.Context, opts []*Options) (*Group, error) {
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
	type opened struct {
		store *cow.Store
		pred  *predecessor
	}
	stores := make(map[string]opened, len(opts))
	closeStores := func() {
		for id, s := range stores {
			closeStore(id, s.store, s.pred)
		}
	}
	for _, o := range opts {
		if _, dup := stores[o.ID]; dup {
			closeStores()
			return nil, fmt.Errorf("duplicate device id %q", o.ID)
		}
		store, pred, err := openStore(o)
		if err != nil {
			closeStores()
			return nil, fmt.Errorf("%s: %w", o.ID, err)
		}
		stores[o.ID] = opened{store: store, pred: pred}
	}
	bases := make([]source.Source, len(opts))
	for i, o := range opts {
		bases[i] = o.Base
	}
	bindAll(bases, func(id string) (io.ReaderAt, bool) {
		s, ok := stores[id]
		if !ok {
			return nil, false
		}
		return s.store, true
	})
	g := &Group{Devices: make(map[string]*Device, len(opts)), failed: make(chan struct{}), closing: make(chan struct{})}
	for _, o := range opts {
		d, err := serveStore(ctx, o, stores[o.ID].store, stores[o.ID].pred)
		if err != nil {
			g.Close()
			for id, s := range stores { // stores of devices that never came up
				if _, up := g.Devices[id]; !up {
					closeStore(id, s.store, s.pred)
				}
			}
			return nil, fmt.Errorf("%s: %w", o.ID, err)
		}
		g.Devices[o.ID] = d
		g.order = append(g.order, o.ID)
	}
	g.watch()
	return g, nil
}

// Close shuts the devices down: first every device's I/O ends (a derived base reads a
// sibling's store, so no store may close while any device still serves), then the devices
// close in reverse order of creation.
func (g *Group) Close() error {
	g.closeOnce.Do(func() { close(g.closing) })
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

// Done is closed when any device of the group fails (see Device.Done): a derived base reads
// its siblings' stores, so one failed store breaks the others too.
// Err says why; after a store failure Abandon the group, otherwise Close it.
func (g *Group) Done() <-chan struct{} {
	return g.failed
}

// Err returns the first device failure, or nil.
func (g *Group) Err() error {
	for _, id := range g.order {
		if err := g.Devices[id].Err(); err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
	}
	return nil
}

// Abandon is Device.Abandon for the whole group: every device's background work stops
// before any store closes (stores read each other), the failed stores drop their live
// bitmaps, and the kernel devices wait for a successor. The caller must exit at once.
func (g *Group) Abandon() error {
	for _, id := range g.order {
		if !g.Devices[id].recovery {
			return ErrNoRecovery
		}
	}
	g.closeOnce.Do(func() { close(g.closing) })
	var errs []error
	for _, id := range g.order {
		d := g.Devices[id]
		if !d.closed.Swap(true) {
			unmarkServed(d.id)
			d.stopBackground()
		}
	}
	for _, id := range g.order {
		errs = append(errs, g.Devices[id].store.Close())
	}
	return errors.Join(errs...)
}

// watch closes failed once any device fails, until the group closes.
func (g *Group) watch() {
	for _, id := range g.order {
		go func(d *Device) {
			select {
			case <-d.Done():
				g.failOnce.Do(func() { close(g.failed) })
			case <-g.closing:
			}
		}(g.Devices[id])
	}
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
