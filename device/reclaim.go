package device

import (
	"context"
	"time"

	"heckel.io/blkmap/cow"
)

const (
	// reclaimBatch bounds the chunks one sweep examines while the guest is idle before its
	// activity is checked again, so a guest that becomes busy waits for at most one batch (256
	// chunks of 64 KiB are 16 MiB read twice).
	reclaimBatch = 256
	// reclaimTrickle is the batch while the guest is busy, once per reclaimPoll: 64 chunks of
	// 64 KiB every 100 ms are 40 MiB/s of overlay reads and as much again from the base (a
	// sibling's store for a derived device, usually still in the page cache), so the overlay
	// stops growing without bound under a sustained write load, which is when it grows fastest.
	// A guest that writes the same bytes to two plexes at 50 MB/s freezes up to 800 chunks a
	// second when its halves land in the wrong order, and 16 chunks a trickle fell behind that.
	reclaimTrickle = 64
	// reclaimIdle is how long the sweeper sleeps when nothing was written since its last pass;
	// reclaimPoll how often it looks again while the guest is busy.
	reclaimIdle = time.Second
	reclaimPoll = hydrateBackoff
)

// sweeper runs cow.Store.Reclaim in the background: it walks the chunks written since it last
// looked, compares each with its base and drops the ones that equal it, so a chunk nopwrite
// froze by timing over a derived base (see cow.Store.Reclaim) stops costing overlay space once
// the siblings agree again. Guest I/O has priority: while nothing is in flight the sweeper
// works through full batches back to back, while the guest is busy it examines a trickle per
// poll, since every chunk it examines costs an overlay read and a base read.
type sweeper struct {
	store *cow.Store
	busy  func() bool
	idle  time.Duration // reclaimIdle, shorter in tests
}

func newSweeper(store *cow.Store, busy func() bool) *sweeper {
	return &sweeper{store: store, busy: busy, idle: reclaimIdle}
}

// run sweeps until ctx ends, which the device's stopBackground does before any store closes.
// An idle batch is followed by another at once while chunks are waiting; a busy trickle by the
// next poll.
func (w *sweeper) run(ctx context.Context) {
	for ctx.Err() == nil {
		wait, budget := w.idle, reclaimBatch
		if w.busy() {
			wait, budget = reclaimPoll, reclaimTrickle
		}
		if w.store.Reclaim(ctx, budget) > 0 && budget == reclaimBatch {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
