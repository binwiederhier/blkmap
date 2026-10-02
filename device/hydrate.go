package device

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"heckel.io/blkmap/config"
	"heckel.io/blkmap/cow"
	"heckel.io/blkmap/source"
	"heckel.io/blkmap/util"
)

const (
	// hydrateBackoff is how long after the last guest request hydration stays paused.
	hydrateBackoff = 100 * time.Millisecond
	hydratePoll    = 50 * time.Millisecond
	// holeWindow bounds one Holes query, so a huge sparse device does not produce one giant
	// range list up front.
	holeWindow = 4 << 30
	// hydrateRunBytes is how much consecutive unwritten data one background read covers, so a
	// remote source sees requests of this size rather than one per chunk.
	hydrateRunBytes = cow.MaxRunBytes
	// hydrateMaxPasses bounds how often failed runs are retried; hydrateRetryDelay separates
	// the passes so a brief source outage is ridden out.
	hydrateMaxPasses  = 5
	hydrateRetryDelay = 10 * time.Second
	phaseList         = "list"
	phaseRest         = "rest"
	phaseDone         = "done"
)

// Hydrate configures background copying of the base into the COW file, so the device
// eventually needs no source at all. Guest I/O always has priority: hydration pauses while
// requests are in flight or arrived in the last hydrateBackoff.
type Hydrate struct {
	Prefetch    []source.Range // hydrated first, in order, as fast as possible
	Rest        bool           // then everything else, paced by Rate
	Rate        int64          // bytes/s for the rest phase; 0 = unlimited
	UseCache    string         // config.CacheAlways (default) or config.CacheNever for the background reads
	Concurrency int            // parallel background reads; default config.DefaultHydrateConcurrency
	Report      time.Duration  // progress log interval; default config.DefaultHydrateReport
	OnProgress  func(Progress) // optional, called at every report and once at the end
}

// Progress is a hydration status snapshot. Done means hydration has ended; whether every
// chunk made it is Hydrated == Total, since a source that stays down ends it with errors.
type Progress struct {
	Phase    string // "list", "rest", or "done"
	Hydrated int64  // chunks in the COW file (written or hydrated)
	Total    int64  // chunks in the device
	Copied   int64  // bytes copied by this run so far
	Errors   int64  // failed background reads so far (each is retried in a later pass)
	Done     bool
}

// hydrator runs the background copy. It is driven entirely by the Store's chunk primitives
// and a busy predicate, so it can be tested without a kernel device.
type hydrator struct {
	store      *cow.Store
	base       source.Source
	opts       *Hydrate
	busy       func() bool
	id         string
	zero       *util.Bitset // chunks that read as zeros: marked, never copied
	phase      atomic.Pointer[string]
	copied     atomic.Int64
	errors     atomic.Int64
	limiter    *bucket
	retryDelay time.Duration
}

// batch is a run of consecutive chunks handed to a worker: zero chunks are marked, unwritten
// chunks are copied with one base read.
type batch struct {
	first, count int64
	zero         bool
}

func newHydrator(id string, store *cow.Store, base source.Source, opts *Hydrate, busy func() bool) *hydrator {
	o := *opts
	if o.Concurrency < 1 {
		o.Concurrency = config.DefaultHydrateConcurrency
	}
	if o.Report <= 0 {
		o.Report = config.DefaultHydrateReport
	}
	if o.UseCache == "" {
		o.UseCache = config.CacheAlways
	}
	h := &hydrator{store: store, base: base, opts: &o, busy: busy, id: id, zero: util.NewBitset(store.Chunks()), retryDelay: hydrateRetryDelay}
	if o.Rate > 0 {
		h.limiter = newBucket(o.Rate, max(hydrateRunBytes, store.ChunkSize()))
	}
	return h
}

// run hydrates until everything requested is in the COW file or ctx is cancelled. Runs
// that failed (the source was down) are retried in later passes, a few times.
func (h *hydrator) run(ctx context.Context) {
	reportCtx, stopReports := context.WithCancel(ctx)
	var reports sync.WaitGroup
	reports.Add(1)
	go func() {
		defer reports.Done()
		h.reportLoop(reportCtx)
	}()
	h.scanHoles()
	for pass := 1; ctx.Err() == nil; pass++ {
		before := h.errors.Load()
		h.pass(ctx)
		failed := h.errors.Load() - before
		if failed == 0 || pass == hydrateMaxPasses {
			break
		}
		log.Printf("%s: hydration pass %d: %d reads failed, retrying in %s", h.id, pass, failed, h.retryDelay)
		select {
		case <-ctx.Done():
		case <-time.After(h.retryDelay):
		}
	}
	stopReports()
	reports.Wait()
	if ctx.Err() != nil {
		return
	}
	h.setPhase(phaseDone)
	p := h.progress()
	log.Printf("%s: hydration done: %d/%d chunks in the cow file, %s copied, %d errors", h.id, p.Hydrated, p.Total, util.FormatSize(p.Copied), p.Errors)
	if h.opts.OnProgress != nil {
		h.opts.OnProgress(p)
	}
}

// scanHoles marks chunks that lie entirely inside holes of the base, which are recorded
// without a copy; the query runs in windows to bound the range lists.
func (h *hydrator) scanHoles() {
	chunkSize := h.store.ChunkSize()
	for off := int64(0); off < h.base.Size(); off += holeWindow {
		holes, err := source.Holes(h.base, off, holeWindow)
		if err != nil {
			log.Printf("%s: cannot query holes at %d: %s (hydrating by copying)", h.id, off, err.Error())
			return
		}
		for _, r := range holes {
			for c := (r.Offset + chunkSize - 1) / chunkSize; c < (r.Offset+r.Length)/chunkSize; c++ {
				h.zero.Set(c)
			}
		}
	}
}

// pass runs the list phase (prefetch ranges in order, each chunk once) and then the rest
// phase (everything else, ascending, paced) over whatever is not yet in the COW file.
func (h *hydrator) pass(ctx context.Context) {
	visited := util.NewBitset(h.store.Chunks())
	h.process(ctx, phaseList, func(yield func(int64) bool) {
		for _, r := range h.opts.Prefetch {
			last := min((r.Offset+r.Length-1)/h.store.ChunkSize(), h.store.Chunks()-1)
			for c := r.Offset / h.store.ChunkSize(); c <= last; c++ {
				if visited.Test(c) {
					continue
				}
				visited.Set(c)
				if !yield(c) {
					return
				}
			}
		}
	}, nil)
	if h.opts.Rest && ctx.Err() == nil {
		h.process(ctx, phaseRest, func(yield func(int64) bool) {
			for c := int64(0); c < h.store.Chunks(); c++ {
				if !visited.Test(c) && !yield(c) {
					return
				}
			}
		}, h.limiter)
	}
}

// process hydrates the chunks produced by next, in order, with the configured concurrency,
// yielding to the guest and to the rate limiter. next is an iterator so the rest phase of a
// huge device never materializes a list of every chunk. Consecutive chunks of the same kind
// are batched into runs of up to hydrateRunBytes.
func (h *hydrator) process(ctx context.Context, phase string, next func(yield func(int64) bool), limiter *bucket) {
	h.setPhase(phase)
	work := make(chan batch)
	var wg sync.WaitGroup
	for i := 0; i < h.opts.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range work {
				h.handle(ctx, b, limiter)
			}
		}()
	}
	maxRun := max(hydrateRunBytes/h.store.ChunkSize(), 1)
	var pending batch
	flush := func() bool {
		if pending.count == 0 {
			return true
		}
		select {
		case work <- pending:
			pending = batch{}
			return true
		case <-ctx.Done():
			return false
		}
	}
	next(func(c int64) bool {
		if h.store.IsWritten(c) {
			return true
		}
		zero := h.zero.Test(c)
		if pending.count > 0 && (c != pending.first+pending.count || zero != pending.zero || pending.count == maxRun) {
			if !flush() {
				return false
			}
		}
		if pending.count == 0 {
			pending = batch{first: c, zero: zero}
		}
		pending.count++
		return true
	})
	flush()
	close(work)
	wg.Wait()
}

func (h *hydrator) handle(ctx context.Context, b batch, limiter *bucket) {
	if !h.waitIdle(ctx) {
		return
	}
	if b.zero {
		for c := b.first; c < b.first+b.count; c++ {
			h.store.MarkZero(c)
		}
		return
	}
	if limiter != nil && !limiter.wait(ctx, b.count*h.store.ChunkSize()) {
		return
	}
	copied, err := h.store.HydrateRun(b.first, b.count, h.opts.UseCache == config.CacheNever)
	h.copied.Add(copied)
	if err != nil {
		if n := h.errors.Add(1); n <= maxLoggedErrors {
			log.Printf("%s: %s", h.id, err.Error())
		}
	}
}

// waitIdle blocks while the guest is active; it returns false if ctx ended meanwhile.
func (h *hydrator) waitIdle(ctx context.Context) bool {
	for h.busy() {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(hydratePoll):
		}
	}
	return true
}

func (h *hydrator) reportLoop(ctx context.Context) {
	ticker := time.NewTicker(h.opts.Report)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p := h.progress()
			log.Printf("%s: hydration %s: %d/%d chunks (%d%%), %s copied", h.id, p.Phase, p.Hydrated, p.Total, 100*p.Hydrated/max(p.Total, 1), util.FormatSize(p.Copied))
			if h.opts.OnProgress != nil {
				h.opts.OnProgress(p)
			}
		}
	}
}

func (h *hydrator) progress() Progress {
	phase := h.phase.Load()
	p := Progress{Hydrated: h.store.Written(), Total: h.store.Chunks(), Copied: h.copied.Load(), Errors: h.errors.Load()}
	if phase != nil {
		p.Phase = *phase
	}
	p.Done = p.Phase == phaseDone
	return p
}

func (h *hydrator) setPhase(phase string) {
	h.phase.Store(&phase)
}

// bucket is a token bucket in bytes with a burst of one chunk, shared by the workers.
type bucket struct {
	rate   int64
	burst  int64
	tokens float64
	last   time.Time
	mu     sync.Mutex // Protects tokens and last
}

func newBucket(rate, burst int64) *bucket {
	return &bucket{rate: rate, burst: burst, tokens: float64(burst), last: time.Now()}
}

// wait blocks until n bytes may proceed; it returns false if ctx ended first.
func (b *bucket) wait(ctx context.Context, n int64) bool {
	for {
		b.mu.Lock()
		now := time.Now()
		b.tokens = min(b.tokens+now.Sub(b.last).Seconds()*float64(b.rate), float64(b.burst))
		b.last = now
		if b.tokens >= float64(n) {
			b.tokens -= float64(n)
			b.mu.Unlock()
			return true
		}
		delay := time.Duration((float64(n) - b.tokens) / float64(b.rate) * float64(time.Second))
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return false
		case <-time.After(delay):
		}
	}
}
