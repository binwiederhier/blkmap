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
	// hydrateMaxLoggedErrors caps per-chunk error lines; the total is logged at the end.
	hydrateMaxLoggedErrors = 20
	phaseList              = "list"
	phaseRest              = "rest"
	phaseDone              = "done"
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

// Progress is a hydration status snapshot.
type Progress struct {
	Phase    string // "list", "rest", or "done"
	Hydrated int64  // chunks in the COW file (written or hydrated)
	Total    int64  // chunks in the device
	Copied   int64  // bytes copied by this run so far
	Done     bool
}

// hydrator runs the background copy. It is driven entirely by the Store's chunk primitives
// and a busy predicate, so it can be tested without a kernel device.
type hydrator struct {
	store   *cow.Store
	base    source.Source
	opts    *Hydrate
	busy    func() bool
	id      string
	zero    map[int64]bool // chunks that read as zeros: marked, never copied
	phase   atomic.Pointer[string]
	copied  atomic.Int64
	errors  atomic.Int64
	limiter *bucket
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
	h := &hydrator{store: store, base: base, opts: &o, busy: busy, id: id, zero: map[int64]bool{}}
	// Chunks entirely inside a zero range need no copy
	chunkSize := store.ChunkSize()
	for _, r := range source.ZeroRanges(base) {
		for c := (r.Offset + chunkSize - 1) / chunkSize; c < (r.Offset+r.Length)/chunkSize; c++ {
			h.zero[c] = true
		}
	}
	if o.Rate > 0 {
		h.limiter = newBucket(o.Rate, chunkSize)
	}
	return h
}

// run hydrates until everything requested is in the COW file or ctx is cancelled.
func (h *hydrator) run(ctx context.Context) {
	reportCtx, stopReports := context.WithCancel(ctx)
	var reports sync.WaitGroup
	reports.Add(1)
	go func() {
		defer reports.Done()
		h.reportLoop(reportCtx)
	}()
	visited := make([]bool, h.store.Chunks())
	var list []int64
	for _, r := range h.opts.Prefetch {
		last := min((r.Offset+r.Length-1)/h.store.ChunkSize(), h.store.Chunks()-1)
		for c := r.Offset / h.store.ChunkSize(); c <= last; c++ {
			if !visited[c] {
				visited[c] = true
				list = append(list, c)
			}
		}
	}
	h.process(ctx, phaseList, list, nil)
	if h.opts.Rest && ctx.Err() == nil {
		var rest []int64
		for c := range visited {
			if !visited[c] {
				rest = append(rest, int64(c))
			}
		}
		h.process(ctx, phaseRest, rest, h.limiter)
	}
	stopReports()
	reports.Wait()
	if ctx.Err() != nil {
		return
	}
	h.setPhase(phaseDone)
	p := h.progress()
	log.Printf("%s: hydration done: %d/%d chunks in the cow file, %s copied, %d errors", h.id, p.Hydrated, p.Total, util.FormatSize(p.Copied), h.errors.Load())
	if h.opts.OnProgress != nil {
		h.opts.OnProgress(p)
	}
}

// process hydrates chunks in order with the configured concurrency, yielding to the guest
// and to the rate limiter.
func (h *hydrator) process(ctx context.Context, phase string, chunks []int64, limiter *bucket) {
	h.setPhase(phase)
	work := make(chan int64)
	var wg sync.WaitGroup
	for i := 0; i < h.opts.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range work {
				h.chunk(ctx, c, limiter)
			}
		}()
	}
	for _, c := range chunks {
		if h.store.IsWritten(c) {
			continue
		}
		select {
		case work <- c:
		case <-ctx.Done():
			close(work)
			wg.Wait()
			return
		}
	}
	close(work)
	wg.Wait()
}

func (h *hydrator) chunk(ctx context.Context, c int64, limiter *bucket) {
	if !h.waitIdle(ctx) {
		return
	}
	if h.zero[c] {
		h.store.MarkZero(c)
		return
	}
	if limiter != nil && !limiter.wait(ctx, h.store.ChunkSize()) {
		return
	}
	copied, err := h.store.HydrateChunk(c, h.opts.UseCache == config.CacheNever)
	if err != nil {
		if n := h.errors.Add(1); n <= hydrateMaxLoggedErrors {
			log.Printf("%s: %s", h.id, err.Error())
		}
		return
	}
	if copied {
		h.copied.Add(h.store.ChunkSize())
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
	p := Progress{Hydrated: h.store.Written(), Total: h.store.Chunks(), Copied: h.copied.Load()}
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
