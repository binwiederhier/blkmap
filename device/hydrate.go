package device

import (
	"context"
	"fmt"
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
	// hydrateBackoff is how long after its last request the guest still counts as active.
	hydrateBackoff = 100 * time.Millisecond
	hydratePoll    = 10 * time.Millisecond
	// behindCheck is how often the share re-checks whether the prefetch list is behind.
	behindCheck = 250 * time.Millisecond
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
// eventually needs no source at all. Guest I/O has priority without starving hydration: while
// the guest is active (requests in flight or within hydrateBackoff) hydration keeps one copy
// running, or half its workers while a timed prefetch list is behind the recording.
type Hydrate struct {
	Prefetch []source.Range // hydrated first, in order, as fast as possible
	// PrefetchAt is when the recorded workload first read each Prefetch range (from a
	// recording's timestamps), so progress can say whether hydration is ahead of the
	// workload or behind it; empty for an untimed list.
	PrefetchAt  []time.Duration
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
	Schedule *Schedule `json:"schedule,omitempty"` // nil without a timed prefetch list
}

// Schedule compares hydration with a timed prefetch list: the list says when the recorded
// workload first read each range, counted from when the device came up, and hydration is
// behind when a range that was due is not in the COW file yet.
type Schedule struct {
	Behind   int64         // listed chunks due by now but not in the COW file
	Lead     time.Duration // ahead (> 0) or behind (< 0) the recording; frozen once the list is complete
	ListDone bool          // every listed chunk is in the COW file
}

func (s *Schedule) String() string {
	lead := s.Lead.Round(100 * time.Millisecond)
	switch {
	case s.ListDone && lead >= 0:
		return fmt.Sprintf("prefetch list complete, %s before the recording needed the last of it", lead)
	case s.ListDone:
		return fmt.Sprintf("prefetch list complete, %s after the recording needed the last of it", -lead)
	case s.Behind > 0:
		return fmt.Sprintf("behind the recording by %s (%d chunks due)", -lead, s.Behind)
	default:
		return fmt.Sprintf("%s ahead of the recording", lead)
	}
}

// hydrator runs the background copy. It is driven entirely by the Store's chunk primitives
// and a busy predicate, so it can be tested without a kernel device.
type hydrator struct {
	store        *cow.Store
	base         source.Source
	opts         *Hydrate
	busy         func() bool
	id           string
	zero         *util.Bitset // chunks that read as zeros: marked, never copied
	phase        atomic.Pointer[string]
	copied       atomic.Int64
	errors       atomic.Int64
	limiter      *bucket
	retryDelay   time.Duration
	start        time.Time        // when the device came up, the recording's time zero
	now          func() time.Time // time.Now, or a test clock
	listDoneLead atomic.Pointer[time.Duration]
	inflight     atomic.Int64 // copies running now
	behindAt     atomic.Int64 // unix nanos of the last behind() check
	isBehind     atomic.Bool
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
	h := &hydrator{store: store, base: base, opts: &o, busy: busy, id: id, zero: util.NewBitset(store.Chunks()), retryDelay: hydrateRetryDelay,
		start: time.Now(), now: time.Now}
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
		if failed == 0 || pass == hydrateMaxPasses || ctx.Err() != nil {
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
				h.handle(ctx, b, limiter, phase)
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

// handle hydrates one batch: zero runs are marked at once, copies wait for a slot in the
// share the guest leaves to hydration (see share).
func (h *hydrator) handle(ctx context.Context, b batch, limiter *bucket, phase string) {
	if b.zero {
		for c := b.first; c < b.first+b.count; c++ {
			h.store.MarkZero(c)
		}
		return
	}
	if limiter != nil && !limiter.wait(ctx, b.count*h.store.ChunkSize()) {
		return
	}
	if !h.admit(ctx, phase) {
		return
	}
	defer h.inflight.Add(-1)
	copied, err := h.store.HydrateRun(b.first, b.count, h.opts.UseCache == config.CacheNever)
	h.copied.Add(copied)
	if err != nil {
		if n := h.errors.Add(1); n <= maxLoggedErrors {
			log.Printf("%s: %s", h.id, err.Error())
		}
	}
}

// share is how many copies may be in flight now. Guest requests come first, but hydration
// never stops: an idle guest leaves it all its workers, a busy one leaves it one, or half of
// them while the prefetch list is behind the recording, since those chunks are the guest's
// own next reads and every one hydration is late for becomes a slow on-demand read.
func (h *hydrator) share(phase string) int64 {
	n := int64(h.opts.Concurrency)
	switch {
	case !h.busy():
		return n
	case phase == phaseList && h.behind():
		return max(n/2, 1)
	default:
		return 1
	}
}

// admit waits for a slot in the share; it returns false if ctx ended first.
func (h *hydrator) admit(ctx context.Context, phase string) bool {
	for {
		n := h.inflight.Load()
		if n < h.share(phase) && h.inflight.CompareAndSwap(n, n+1) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(hydratePoll):
		}
	}
}

// behind reports whether the timed prefetch list is behind the recording, re-checked at most
// every behindCheck (it walks the whole list).
func (h *hydrator) behind() bool {
	now := h.now().UnixNano()
	if last := h.behindAt.Load(); now-last >= int64(behindCheck) && h.behindAt.CompareAndSwap(last, now) {
		s := h.schedule()
		h.isBehind.Store(s != nil && s.Behind > 0)
	}
	return h.isBehind.Load()
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
			line := fmt.Sprintf("%s: hydration %s: %d/%d chunks (%d%%), %s copied", h.id, p.Phase, p.Hydrated, p.Total, 100*p.Hydrated/max(p.Total, 1), util.FormatSize(p.Copied))
			if p.Schedule != nil {
				line += "; " + p.Schedule.String()
			}
			log.Print(line)
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
	p.Schedule = h.schedule()
	return p
}

// schedule compares the listed chunks in the COW file with when the recording read them;
// nil without a timed list. It walks the whole list, which is cheap next to a report.
func (h *hydrator) schedule() *Schedule {
	at := h.opts.PrefetchAt
	if len(at) == 0 || len(at) != len(h.opts.Prefetch) {
		return nil
	}
	if done := h.listDoneLead.Load(); done != nil {
		return &Schedule{Lead: *done, ListDone: true}
	}
	elapsed := h.now().Sub(h.start)
	chunkSize, chunks := h.store.ChunkSize(), h.store.Chunks()
	s := &Schedule{}
	oldest, next, last := time.Duration(-1), time.Duration(-1), time.Duration(0)
	for i, r := range h.opts.Prefetch {
		last = max(last, at[i])
		var missing int64
		for c := r.Offset / chunkSize; c <= min((r.Offset+r.Length-1)/chunkSize, chunks-1); c++ {
			if !h.store.IsWritten(c) {
				missing++
			}
		}
		switch {
		case missing == 0:
		case at[i] <= elapsed:
			s.Behind += missing
			if oldest < 0 || at[i] < oldest {
				oldest = at[i]
			}
		case next < 0 || at[i] < next:
			next = at[i]
		}
	}
	switch {
	case s.Behind > 0:
		s.Lead = oldest - elapsed
	case next >= 0:
		s.Lead = next - elapsed
	default:
		s.ListDone, s.Lead = true, last-elapsed
		h.listDoneLead.CompareAndSwap(nil, &s.Lead)
	}
	return s
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
