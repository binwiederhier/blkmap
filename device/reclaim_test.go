package device

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The sweeper drops a stored chunk that equals its base while the guest is idle, and stops
// when its context ends.
func TestSweeperReclaimsWhenIdle(t *testing.T) {
	t.Parallel()
	data := seeded(8*groupChunk, 3)
	s := openTestStore(t, t.TempDir(), "sw", data)
	s.EnableReclaim()
	_, err := s.WriteAt(data[groupChunk:2*groupChunk], groupChunk) // nopwrite is off: stored
	require.NoError(t, err)
	_, err = s.WriteAt([]byte{1, 2, 3}, 3*groupChunk)
	require.NoError(t, err)
	require.EqualValues(t, 2, s.Written())
	w := newSweeper(s, idle)
	w.idle = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.run(ctx)
		close(done)
	}()
	require.Eventually(t, func() bool { return s.Written() == 1 }, 2*time.Second, 5*time.Millisecond, "the identical chunk is dropped, the different one kept")
	rs := s.ReclaimStats()
	assert.Equal(t, int64(2), rs.Examined)
	assert.Equal(t, int64(1), rs.Chunks)
	assert.Equal(t, int64(groupChunk), rs.Bytes)
	assert.EqualValues(t, 1, rs.Pending, "the differing chunk waits for its second look")
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sweeper did not stop on cancel")
	}
}

// A busy guest slows the sweeper to a trickle of reclaimTrickle chunks per poll, it does not
// stop it: the chunks are dropped over several polls, never more than a trickle per poll.
// Cancelling still stops it promptly.
func TestSweeperTricklesUnderLoad(t *testing.T) {
	t.Parallel()
	const chunks = 3 * reclaimTrickle
	data := seeded(chunks*groupChunk, 4)
	s := openTestStore(t, t.TempDir(), "sb", data)
	s.EnableReclaim()
	for c := range int64(chunks) { // identical whole-chunk writes: stored, since nopwrite is off
		_, err := s.WriteAt(data[c*groupChunk:(c+1)*groupChunk], c*groupChunk)
		require.NoError(t, err)
	}
	require.EqualValues(t, chunks, s.Written())
	w := newSweeper(s, func() bool { return true })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	start := time.Now()
	go func() {
		w.run(ctx)
		close(done)
	}()
	last := int64(chunks)
	require.Eventually(t, func() bool {
		if n := s.Written(); n < last {
			// two polls can fall between two looks here, a full batch cannot
			assert.LessOrEqual(t, last-n, int64(2*reclaimTrickle), "at most a trickle per poll")
			last = n
		}
		return last == 0
	}, 5*time.Second, time.Millisecond, "the sweeper makes progress while the guest stays busy")
	assert.GreaterOrEqual(t, time.Since(start), 2*reclaimPoll, "three trickles take at least two polls")
	assert.Equal(t, int64(chunks), s.ReclaimStats().Examined)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sweeper did not stop on cancel")
	}
}

// Reclaim drops chunks that equal the base, hydration copies them: Serve refuses the pair
// before anything is opened.
func TestServeRefusesReclaimWithHydrate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := Serve(context.Background(), &Options{ID: "rh", Base: &bytesSource{data: make([]byte, 1<<20)}, COWFile: filepath.Join(dir, "rh.cow"), RunDir: dir, DevDir: dir,
		Reclaim: true, Hydrate: &Hydrate{Rest: true}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "contradict")
}

// A device served with Reclaim starts the sweeper, reports its counters, and closes cleanly
// (the sweeper is stopped with the other background work before the store closes).
func TestServeReclaim(t *testing.T) {
	requireUblk(t)
	dir := t.TempDir()
	d, err := Serve(context.Background(), &Options{
		ID:       "reclaim",
		Base:     &bytesSource{data: seeded(8<<20, 5)},
		COWFile:  filepath.Join(dir, "reclaim.cow"),
		DevDir:   filepath.Join(dir, "dev"),
		RunDir:   dir,
		NopWrite: true,
		Reclaim:  true,
	})
	require.NoError(t, err)
	require.NotNil(t, d.sweeper)
	st := d.Status()
	require.NotNil(t, st.Reclaim)
	assert.Zero(t, st.Reclaim.Pending)
	closed := make(chan error, 1)
	go func() { closed <- d.Close() }()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return")
	}
}
