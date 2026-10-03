package device

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"heckel.io/blkmap/config"
	"heckel.io/blkmap/cow"
	"heckel.io/blkmap/source"
)

const (
	hChunk = 4096
	hSize  = 64 * hChunk
)

// recorder is a base that records the order of its reads and distinguishes direct reads.
type recorder struct {
	data   []byte
	reads  []int64 // offsets in order
	direct int
	mu     sync.Mutex
}

func (r *recorder) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	r.reads = append(r.reads, off)
	r.mu.Unlock()
	return copy(p, r.data[off:]), nil
}

func (r *recorder) ReadAtDirect(p []byte, off int64) (int, error) {
	r.mu.Lock()
	r.direct++
	r.mu.Unlock()
	return r.ReadAt(p, off)
}

func (r *recorder) Size() int64 {
	return int64(len(r.data))
}

func (r *recorder) Close() error {
	return nil
}

func (r *recorder) offsets() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.reads...)
}

func pat(n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(i*7 + i/256)
	}
	return p
}

func newHydrateStore(t *testing.T, base source.Source) *cow.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := cow.Open(base, filepath.Join(dir, "h.cow"), filepath.Join(dir, "h.cow.bitmap"), hChunk)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func idle() bool {
	return false
}

func TestHydratorListThenRest(t *testing.T) {
	t.Parallel()
	base := &recorder{data: pat(hSize)}
	s := newHydrateStore(t, base)
	// Chunk 10 was written by the guest and must be left alone
	_, err := s.WriteAt(bytes.Repeat([]byte{'g'}, hChunk), 10*hChunk)
	require.NoError(t, err)
	var reports []Progress
	h := newHydrator("h", s, base, &Hydrate{
		Prefetch:    []source.Range{{Offset: 20 * hChunk, Length: 2 * hChunk}, {Offset: 5*hChunk + 100, Length: 100}, {Offset: 20 * hChunk, Length: hChunk}},
		Rest:        true,
		Concurrency: 1,
		Report:      10 * time.Millisecond,
		OnProgress:  func(p Progress) { reports = append(reports, p) },
	}, idle)
	h.run(context.Background())
	assert.Equal(t, s.Chunks(), s.Written())
	// Listed chunks first (20+21 as one run, then 5; the duplicate 20 skipped), then the
	// rest ascending in runs that stop at listed or written chunks: 0..4, 6..9, 11..19, 22..
	offs := base.offsets()
	assert.Equal(t, []int64{20 * hChunk, 5 * hChunk, 0, 6 * hChunk, 11 * hChunk, 22 * hChunk}, offs[:6])
	assert.NotContains(t, offs, int64(10*hChunk))
	assert.Equal(t, 0, base.direct)
	// Content: base everywhere except the guest write
	expected := pat(hSize)
	copy(expected[10*hChunk:], bytes.Repeat([]byte{'g'}, hChunk))
	got := make([]byte, hSize)
	_, err = s.ReadAt(got, 0)
	require.NoError(t, err)
	assert.Equal(t, expected, got)
	require.NotEmpty(t, reports)
	last := reports[len(reports)-1]
	assert.True(t, last.Done)
	assert.Equal(t, "done", last.Phase)
	assert.Equal(t, int64(64), last.Hydrated)
	assert.Equal(t, int64(64), last.Total)
	assert.Equal(t, int64(63*hChunk), last.Copied)
}

func TestHydratorListOnlyDirect(t *testing.T) {
	t.Parallel()
	base := &recorder{data: pat(hSize)}
	s := newHydrateStore(t, base)
	h := newHydrator("h", s, base, &Hydrate{
		Prefetch: []source.Range{{Offset: 0, Length: 3 * hChunk}},
		Rest:     false,
		UseCache: config.CacheNever,
	}, idle)
	h.run(context.Background())
	assert.Equal(t, int64(3), s.Written())
	assert.Equal(t, 1, base.direct) // one read for the whole run
}

func TestHydratorZeroRangesAndConcurrency(t *testing.T) {
	t.Parallel()
	file := &recorder{data: pat(16 * hChunk)}
	base, err := source.NewConcat([]*source.Segment{
		{Offset: 0, Source: source.NewZero(8 * hChunk)},
		{Offset: 8 * hChunk, Source: file},
		{Offset: 32 * hChunk, Source: source.NewZero(hChunk / 2)}, // gap 24..32, partial zero chunk 32
	}, hSize)
	require.NoError(t, err)
	s := newHydrateStore(t, base)
	h := newHydrator("h", s, base, &Hydrate{Rest: true, Concurrency: 4}, idle)
	h.run(context.Background())
	assert.Equal(t, s.Chunks(), s.Written())
	// Only the file chunks and the half-zero chunk were read; zero chunks were marked. The
	// 16 file chunks arrive as runs, never one read per chunk
	assert.Less(t, len(file.offsets()), 16)
	got := make([]byte, hSize)
	_, err = s.ReadAt(got, 0)
	require.NoError(t, err)
	expected := make([]byte, hSize)
	copy(expected[8*hChunk:], pat(16*hChunk))
	assert.Equal(t, expected, got)
	// The COW file stayed sparse for the zero chunks: fewer blocks than a full copy
	st, err := s.Stat()
	require.NoError(t, err)
	assert.Less(t, st.Blocks*512, int64(32*hChunk))
}

func TestHydratorRateAndBusy(t *testing.T) {
	t.Parallel()
	// Reads come in 1 MiB runs and the limiter allows one run up front, so a 4 MiB device at
	// 2 MiB/s should take about 1.5 s; check it is clearly paced
	base := &recorder{data: pat(4 << 20)}
	s := newHydrateStore(t, base)
	start := time.Now()
	h := newHydrator("h", s, base, &Hydrate{Rest: true, Rate: 2 << 20}, idle)
	h.run(context.Background())
	assert.Greater(t, time.Since(start), 1200*time.Millisecond)
	assert.Less(t, time.Since(start), 3*time.Second)
	assert.Equal(t, s.Chunks(), s.Written())
	// Busy guest: nothing happens until it goes idle
	base2 := &recorder{data: pat(hSize)}
	s2 := newHydrateStore(t, base2)
	var busy atomic.Bool
	busy.Store(true)
	done := make(chan struct{})
	go func() {
		newHydrator("h", s2, base2, &Hydrate{Rest: true}, busy.Load).run(context.Background())
		close(done)
	}()
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, int64(0), s2.Written())
	busy.Store(false)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hydration did not resume after the guest went idle")
	}
	assert.Equal(t, s2.Chunks(), s2.Written())
}

func TestHydratorCancel(t *testing.T) {
	t.Parallel()
	base := &recorder{data: pat(8 << 20)}
	s := newHydrateStore(t, base)
	ctx, cancel := context.WithCancel(context.Background())
	h := newHydrator("h", s, base, &Hydrate{Rest: true, Rate: hChunk}, idle) // one 1 MiB run, then a crawl
	done := make(chan struct{})
	go func() {
		h.run(ctx)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("hydrator did not stop on cancel")
	}
	assert.Less(t, s.Written(), int64(300)) // the first run (256 chunks) plus little else
}

// flaky is a base whose reads fail until a number of attempts have been made.
type flaky struct {
	recorder
	failures  atomic.Int32
	remaining atomic.Int32
}

func (f *flaky) ReadAt(p []byte, off int64) (int, error) {
	if f.remaining.Add(-1) >= 0 {
		f.failures.Add(1)
		return 0, errors.New("origin down")
	}
	return f.recorder.ReadAt(p, off)
}

func TestHydratorRetriesFailedRuns(t *testing.T) {
	t.Parallel()
	base := &flaky{recorder: recorder{data: pat(hSize)}}
	base.remaining.Store(3)
	s := newHydrateStore(t, base)
	var last Progress
	h := newHydrator("h", s, base, &Hydrate{Rest: true, Concurrency: 2, OnProgress: func(p Progress) { last = p }}, idle)
	h.retryDelay = time.Millisecond
	h.run(context.Background())
	assert.Equal(t, s.Chunks(), s.Written(), "failed runs are retried in a later pass")
	assert.True(t, last.Done)
	assert.Equal(t, int64(3), last.Errors)
	assert.Equal(t, int32(3), base.failures.Load())
	got := make([]byte, hSize)
	_, err := s.ReadAt(got, 0)
	require.NoError(t, err)
	assert.Equal(t, pat(hSize), got)
}

func TestHydratorGivesUpAfterMaxPasses(t *testing.T) {
	t.Parallel()
	base := &flaky{recorder: recorder{data: pat(hSize)}}
	base.remaining.Store(1 << 30)
	s := newHydrateStore(t, base)
	var last Progress
	h := newHydrator("h", s, base, &Hydrate{Rest: true, Concurrency: 1, OnProgress: func(p Progress) { last = p }}, idle)
	h.retryDelay = time.Millisecond
	h.run(context.Background())
	assert.Equal(t, int64(0), s.Written())
	assert.True(t, last.Done, "hydration ends even when the source never recovers")
	assert.Positive(t, last.Errors)
	assert.LessOrEqual(t, base.failures.Load(), int32(hydrateMaxPasses*(hSize/hydrateRunBytes+1)), "bounded passes")
}

// blockedSource blocks every read until aborted.
type blockedSource struct {
	started, release chan struct{}
	once, aborted    sync.Once
}

func (s *blockedSource) Size() int64 { return 65536 }
func (s *blockedSource) ReadAt(p []byte, off int64) (int, error) {
	s.once.Do(func() { close(s.started) })
	<-s.release
	return 0, errors.New("aborted")
}
func (s *blockedSource) Abort()       { s.aborted.Do(func() { close(s.release) }) }
func (s *blockedSource) Close() error { s.Abort(); return nil }

// A hydration read blocked in the source must hold up neither a handoff nor a shutdown: the
// handoff does not wait for it (the successor re-attaches; the exec ends the read), and
// shutdown aborts the source before it waits.
func TestDetachAndHaltDoNotWaitOnBlockedHydration(t *testing.T) {
	for _, op := range []string{"detach", "halt"} {
		t.Run(op, func(t *testing.T) {
			dir := t.TempDir()
			base := &blockedSource{started: make(chan struct{}), release: make(chan struct{})}
			s, err := cow.Open(base, filepath.Join(dir, "cow"), filepath.Join(dir, "bitmap"), 65536)
			require.NoError(t, err)
			defer s.Close()
			ctx, cancel := context.WithCancel(context.Background())
			d := &Device{store: s, stop: cancel, ublk: nil}
			h := newHydrator("t", s, base, &Hydrate{Rest: true, Concurrency: 1}, idle)
			d.bg.Add(1)
			go func() { defer d.bg.Done(); h.run(ctx) }()
			select {
			case <-base.started:
			case <-time.After(time.Second):
				t.Fatal("hydration did not start")
			}
			done := make(chan struct{})
			go func() {
				if op == "detach" {
					d.Detach()
				} else {
					d.stopBackground()
				}
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				base.Abort()
				<-done
				t.Fatalf("%s waited for a hydration read blocked in the source", op)
			}
			base.Abort()
		})
	}
}

func TestHydratorSchedule(t *testing.T) {
	t.Parallel()
	base := &recorder{data: pat(hSize)}
	s := newHydrateStore(t, base)
	start := time.Now()
	now := start
	h := newHydrator("h", s, base, &Hydrate{
		Prefetch:   []source.Range{{Offset: 0, Length: hChunk}, {Offset: hChunk, Length: 2 * hChunk}, {Offset: 5 * hChunk, Length: hChunk}},
		PrefetchAt: []time.Duration{0, time.Second, 3 * time.Second},
	}, idle)
	h.start, h.now = start, func() time.Time { return now }
	_, err := s.HydrateRun(0, 1, false)
	require.NoError(t, err)
	// At 2 s, chunks 1 and 2 were needed a second ago and are not there yet
	now = start.Add(2 * time.Second)
	sch := h.progress().Schedule
	require.NotNil(t, sch)
	assert.Equal(t, int64(2), sch.Behind)
	assert.Equal(t, -time.Second, sch.Lead)
	assert.Equal(t, "behind the recording by 1s (2 chunks due)", sch.String())
	// Copied: the next range is due at 3 s, a second away
	_, err = s.HydrateRun(1, 2, false)
	require.NoError(t, err)
	sch = h.progress().Schedule
	assert.Equal(t, int64(0), sch.Behind)
	assert.Equal(t, time.Second, sch.Lead)
	assert.Equal(t, "1s ahead of the recording", sch.String())
	// Everything listed is there
	_, err = s.HydrateRun(5, 1, false)
	require.NoError(t, err)
	sch = h.progress().Schedule
	assert.True(t, sch.ListDone)
	assert.Equal(t, "prefetch list complete, 1s before the recording needed the last of it", sch.String())
	// An untimed list has no schedule
	h2 := newHydrator("h", s, base, &Hydrate{Prefetch: []source.Range{{Offset: 0, Length: hChunk}}}, idle)
	assert.Nil(t, h2.progress().Schedule)
}

// cancelling is a base whose reads fail and cancel the hydration, as a shutdown's abort does.
type cancelling struct {
	recorder
	cancel context.CancelFunc
}

func (c *cancelling) ReadAt(p []byte, off int64) (int, error) {
	c.cancel()
	return 0, errors.New("aborted")
}

func TestHydratorDoesNotRetryAfterCancel(t *testing.T) {
	var out bytes.Buffer
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	ctx, cancel := context.WithCancel(context.Background())
	base := &cancelling{recorder: recorder{data: pat(hSize)}, cancel: cancel}
	s := newHydrateStore(t, base)
	h := newHydrator("h", s, base, &Hydrate{Rest: true, Concurrency: 1}, idle)
	h.retryDelay = time.Millisecond
	h.run(ctx)
	assert.NotContains(t, out.String(), "retrying", "reads failing because the device stops are not a pass to retry")
}
