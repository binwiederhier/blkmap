package main

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"heckel.io/blkmap/cow"
	"heckel.io/blkmap/device"
	"heckel.io/blkmap/source"
)

// TestElisionSkipsIdenticalWrites shows the store-level behaviour the mirror relies on, no
// kernel needed: with elision on, rewriting what the device already reads stores nothing.
func TestElisionSkipsIdenticalWrites(t *testing.T) {
	data := make([]byte, 4<<20)
	_, _ = rand.Read(data)
	path := filepath.Join(t.TempDir(), "disk0.img")
	require.NoError(t, os.WriteFile(path, data, 0600))
	base, err := source.OpenFile(path, 0, 0)
	require.NoError(t, err)
	dir := t.TempDir()
	s, err := cow.Open(base, filepath.Join(dir, "c.cow"), filepath.Join(dir, "c.bitmap"), 64<<10)
	require.NoError(t, err)
	defer s.Close()
	s.SetElision(true)
	_, err = s.WriteAt(data[1<<20:2<<20], 1<<20) // a resync: the same bytes
	require.NoError(t, err)
	assert.Equal(t, int64(0), s.Written(), "identical writes store nothing")
	_, err = s.WriteAt([]byte("changed"), 1<<20)
	require.NoError(t, err)
	assert.Equal(t, int64(1), s.Written(), "a real change stores one chunk")
}

// TestMirrorThroughKernel serves the two disks and runs the demo's resync (root and
// ublk_drv only).
func TestMirrorThroughKernel(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if _, err := os.Stat("/dev/ublk-control"); err != nil {
		t.Skip("needs ublk_drv")
	}
	dir := t.TempDir()
	data := make([]byte, 64<<20)
	_, _ = rand.Read(data)
	disk0 := filepath.Join(dir, "disk0.img")
	require.NoError(t, os.WriteFile(disk0, data, 0600))
	m := &mirror{disk0: disk0, dataOffset: 1 << 20, dataLength: defaultDataLength(64<<20, 1<<20), stateDir: dir}
	opts, err := m.groupOptions()
	require.NoError(t, err)
	for _, o := range opts {
		o.DevDir, o.RunDir = filepath.Join(dir, "dev"), filepath.Join(dir, "run")
	}
	g, err := device.ServeGroup(context.Background(), opts)
	require.NoError(t, err)
	defer g.Close()
	d0, d1 := g.Devices["ldm0"], g.Devices["ldm1"]
	// Disk 1's data range reads disk 0's bytes, its header reads zeros
	f1, err := os.Open(d1.BlockPath)
	require.NoError(t, err)
	got := make([]byte, 4096)
	_, err = f1.ReadAt(got, 5<<20)
	require.NoError(t, err)
	assert.Equal(t, data[5<<20:5<<20+4096], got)
	_, err = f1.ReadAt(got, 0)
	require.NoError(t, err)
	assert.Equal(t, make([]byte, 4096), got)
	require.NoError(t, f1.Close())
	require.NoError(t, showElision(d0, d1, 1<<20))
	assert.Equal(t, int64(1), d0.Written()+d1.Written(), "the resync stored nothing, the change one chunk")
}
