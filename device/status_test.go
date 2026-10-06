package device

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"heckel.io/blkmap/cow"
)

func TestBackendCounts(t *testing.T) {
	dir := t.TempDir()
	store, err := cow.Open(&failing{size: 1 << 20}, filepath.Join(dir, "d.cow"), filepath.Join(dir, "d.cow.bitmap"), 4096)
	require.NoError(t, err)
	defer store.Close()
	b := &backend{store: store, id: "d"}
	log := captureLog(t)
	_, err = b.WriteAt(make([]byte, 4096), 0)
	require.NoError(t, err)
	_, err = b.ReadAt(make([]byte, 4096), 0) // written: served from the cow file
	require.NoError(t, err)
	_, err = b.ReadAt(make([]byte, 8192), 8192) // unwritten: the base fails
	require.Error(t, err)
	require.NoError(t, b.Flush())
	require.NoError(t, b.Discard(4096, 4096))
	require.NoError(t, b.WriteZeroes(4096, 4096))
	_ = log
	st := b.stats()
	assert.Equal(t, int64(2), st.Reads)
	assert.Equal(t, int64(1), st.Writes)
	assert.Equal(t, int64(1), st.Flushes)
	assert.Equal(t, int64(1), st.Discards)
	assert.Equal(t, int64(1), st.WriteZeroes)
	assert.Equal(t, int64(4096), st.ReadBytes, "failed reads move no bytes")
	assert.Equal(t, int64(4096), st.WriteBytes)
	assert.Equal(t, int64(1), st.Errors)
	assert.Zero(t, st.Inflight)
	assert.Equal(t, int64(2), st.ReadLatency.Count)
	assert.Equal(t, int64(1), st.WriteLatency.Count)
	assert.Len(t, st.ReadLatency.Counts, len(latencyBucketNanos)+1)
	p := make([]byte, 4096)
	assert.Zero(t, testing.AllocsPerRun(100, func() { b.ReadAt(p, 0) }), "counting must not allocate")
}

func TestWriteMetrics(t *testing.T) {
	st := &Status{ID: "disk1", Size: 1 << 30, Chunks: 16384, Written: 10, Queues: 2, ParallelQueues: 1, Recovered: true,
		IO: IOStats{Reads: 5, ReadBytes: 20480, ReadLatency: Histogram{Counts: make([]int64, len(latencyBucketNanos)+1), Count: 5, Sum: 0.01}}}
	st.IO.ReadLatency.Counts[0] = 5
	st.Hydration = &Progress{Phase: "rest", Hydrated: 100, Total: 16384, Copied: 1 << 20}
	var buf bytes.Buffer
	require.NoError(t, WriteMetrics(&buf, st))
	out := buf.String()
	assert.Contains(t, out, `blkmap_up{device="disk1"} 1`)
	assert.Contains(t, out, `blkmap_size_bytes{device="disk1"} 1.073741824e+09`)
	assert.Contains(t, out, `blkmap_chunks_written{device="disk1"} 10`)
	assert.Contains(t, out, `blkmap_requests_total{device="disk1",op="read"} 5`)
	assert.Contains(t, out, `blkmap_request_duration_seconds_bucket{device="disk1",op="read",le="+Inf"} 5`)
	assert.Contains(t, out, `blkmap_hydration_chunks{device="disk1"} 100`)
	assert.Contains(t, out, `blkmap_recovered{device="disk1"} 1`)
	sample := regexp.MustCompile(`^blkmap_[a-z_]+\{[^}]*\} [-+0-9.e]+$`)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.HasPrefix(line, "#") {
			assert.Regexp(t, sample, line)
		}
	}
	// Each metric's HELP/TYPE appears once even for several devices
	buf.Reset()
	require.NoError(t, WriteMetrics(&buf, st, &Status{ID: "disk2"}))
	assert.Equal(t, 1, strings.Count(buf.String(), "# TYPE blkmap_up gauge"))
	assert.Contains(t, buf.String(), `blkmap_up{device="disk2"} 1`)
}

func TestStatusSocket(t *testing.T) {
	dir := t.TempDir()
	want := &Status{ID: "disk1", Written: 7}
	l, err := ListenStatus(filepath.Join(dir, "disk1"+statusSocketExt), func() *Status { return want })
	require.NoError(t, err)
	st, err := os.Stat(filepath.Join(dir, "disk1"+statusSocketExt))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), st.Mode().Perm(), "root only")
	got, err := QueryStatus(dir, "disk1")
	require.NoError(t, err)
	assert.Equal(t, int64(7), got.Written)
	all, err := QueryAll(dir)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, "disk1", all[0].ID)
	// The same socket answers Prometheus text
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return net.Dial("unix", filepath.Join(dir, "disk1"+statusSocketExt))
	}}}
	resp, err := client.Get("http://blkmap/metrics")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Contains(t, string(body), `blkmap_up{device="disk1"} 1`)
	resp, err = client.Get("http://blkmap/status")
	require.NoError(t, err)
	var decoded Status
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&decoded))
	resp.Body.Close()
	assert.Equal(t, "disk1", decoded.ID)
	require.NoError(t, l.Close())
	_, err = os.Stat(filepath.Join(dir, "disk1"+statusSocketExt))
	assert.True(t, os.IsNotExist(err))
	// A stale socket of a dead server is skipped, not an error for the listing
	stale, err := net.Listen("unix", filepath.Join(dir, "gone"+statusSocketExt))
	require.NoError(t, err)
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	all, err = QueryAll(dir)
	require.NoError(t, err)
	assert.Empty(t, all)
	_ = time.Second
}

// captureLog silences the log for the test and returns what was written.
func captureLog(t *testing.T) *bytes.Buffer {
	var out bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &out
}

func TestQueryStatusReleasesConnections(t *testing.T) {
	// Polling from a long-running process must not keep one socket pair per query alive
	dir := t.TempDir()
	server, err := ListenStatus(filepath.Join(dir, "poll"+statusSocketExt), func() *Status { return &Status{ID: "poll"} })
	require.NoError(t, err)
	t.Cleanup(func() { server.Close() })
	fds := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		require.NoError(t, err)
		return len(entries)
	}
	before := fds()
	const polls = 40
	for range polls {
		_, err := QueryStatus(dir, "poll")
		require.NoError(t, err)
	}
	require.Eventually(t, func() bool { return fds()-before < 4 }, 2*time.Second, 20*time.Millisecond,
		"descriptors grew by %d over %d polls", fds()-before, polls)
}
