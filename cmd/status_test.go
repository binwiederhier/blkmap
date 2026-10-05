package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"heckel.io/blkmap/cow"
	"heckel.io/blkmap/device"
	"path/filepath"
)

func TestStatusAndMetrics(t *testing.T) {
	dir := t.TempDir()
	st := &device.Status{ID: "disk1", Path: "/dev/blkmap/disk1", BlockPath: "/dev/ublkb3", Size: 1 << 30, Chunks: 16384, Written: 4096, Queues: 2,
		Hydration: &device.Progress{Phase: "rest", Hydrated: 4096, Total: 16384, Copied: 256 << 20},
		Reclaim:   &cow.ReclaimStats{Pending: 7, Examined: 40, Chunks: 3, Bytes: 192 << 10}}
	l, err := device.ListenStatus(filepath.Join(dir, "disk1.sock"), func() *device.Status { return st })
	require.NoError(t, err)
	defer l.Close()
	app, stdout, _ := newTestApp()
	require.NoError(t, app.Run([]string{"blkmap", "status", "--run-dir", dir}))
	out := stdout.String()
	assert.Contains(t, out, "disk1")
	assert.Contains(t, out, "/dev/ublkb3")
	assert.Contains(t, out, "25%") // 4096 of 16384 chunks in the cow file
	assert.Contains(t, out, "hydration rest")
	assert.Contains(t, out, "reclaim: 3 chunks (192K) dropped, 40 examined, 7 pending")
	app, stdout, _ = newTestApp()
	require.NoError(t, app.Run([]string{"blkmap", "status", "--run-dir", dir, "--json", "disk1"}))
	assert.Contains(t, stdout.String(), `"written":4096`)
	app, _, _ = newTestApp()
	require.Error(t, app.Run([]string{"blkmap", "status", "--run-dir", dir, "nope"}))
	app, stdout, _ = newTestApp()
	require.NoError(t, app.Run([]string{"blkmap", "metrics", "--run-dir", dir}))
	assert.Contains(t, stdout.String(), `blkmap_up{device="disk1"} 1`)
}
