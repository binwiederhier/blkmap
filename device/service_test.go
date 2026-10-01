package device

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"heckel.io/blkmap/config"
)

const (
	ublkControl = "/dev/ublk-control"
)

func pattern(n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(i*7 + i/256)
	}
	return p
}

// requireUblk skips unless the test can actually create ublk devices (root + ublk_drv loaded).
func requireUblk(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if _, err := os.Stat(ublkControl); err != nil {
		t.Skip("needs ublk_drv loaded (" + ublkControl + " missing)")
	}
}

func TestStart(t *testing.T) {
	requireUblk(t)
	dir := t.TempDir()
	img := filepath.Join(dir, "img")
	require.NoError(t, os.WriteFile(img, pattern(1<<20), 0600))
	c, err := config.Parse("test", []byte(`
size: 4M
cow:
  file: `+dir+`/test.cow
segments:
  - type: zero
    size: 1M
  - type: file
    path: `+img+`
`))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	devDir := filepath.Join(dir, "dev")
	d, err := Start(ctx, c, devDir)
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	// The symlink is published and points at the kernel device
	assert.Equal(t, filepath.Join(devDir, "test"), d.Path)
	target, err := os.Readlink(d.Path)
	require.NoError(t, err)
	assert.Equal(t, d.BlockPath, target)
	assert.Regexp(t, `^/dev/ublkb\d+$`, d.BlockPath)
	// Size and stitched content as seen through the kernel
	f, err := os.OpenFile(d.Path, os.O_RDWR, 0)
	require.NoError(t, err)
	t.Cleanup(func() { f.Close() })
	size, err := f.Seek(0, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(4<<20), size)
	buf := make([]byte, 4<<20)
	_, err = f.ReadAt(buf, 0)
	require.NoError(t, err)
	assert.Equal(t, make([]byte, 1<<20), buf[:1<<20])
	assert.Equal(t, pattern(1<<20), buf[1<<20:2<<20])
	assert.Equal(t, make([]byte, 2<<20), buf[2<<20:])
	// A write lands in the COW file, not the image, and reads back through the device
	data := bytes.Repeat([]byte("blkmap!!"), 512) // 4 KiB
	_, err = f.WriteAt(data, 1<<20+8192)
	require.NoError(t, err)
	require.NoError(t, f.Sync())
	got := make([]byte, 4096)
	_, err = f.ReadAt(got, 1<<20+8192)
	require.NoError(t, err)
	assert.Equal(t, data, got)
	assert.Equal(t, int64(1), d.Written())
	imgNow, err := os.ReadFile(img)
	require.NoError(t, err)
	assert.Equal(t, pattern(1<<20), imgNow)
	cow, err := os.ReadFile(filepath.Join(dir, "test.cow"))
	require.NoError(t, err)
	assert.Equal(t, data, cow[1<<20+8192:1<<20+8192+4096])
	// Close removes the symlink and the kernel device
	require.NoError(t, f.Close())
	require.NoError(t, d.Close())
	_, err = os.Lstat(d.Path)
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(d.BlockPath)
	assert.True(t, os.IsNotExist(err))
}

func TestStartReadOnly(t *testing.T) {
	requireUblk(t)
	dir := t.TempDir()
	c, err := config.Parse("ro", []byte(`
read-only: true
cow:
  file: `+dir+`/ro.cow
segments:
  - type: zero
    size: 1M
`))
	require.NoError(t, err)
	d, err := Start(context.Background(), c, filepath.Join(dir, "dev"))
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	f, err := os.OpenFile(d.Path, os.O_RDWR, 0)
	require.NoError(t, err)
	t.Cleanup(func() { f.Close() })
	ro, err := unix.IoctlGetInt(int(f.Fd()), unix.BLKROGET)
	require.NoError(t, err)
	assert.Equal(t, 1, ro)
	_, err = f.WriteAt(make([]byte, 512), 0)
	require.Error(t, err)
	buf := make([]byte, 512)
	_, err = f.ReadAt(buf, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(0), d.Written())
}

func TestStartStaleSymlink(t *testing.T) {
	requireUblk(t)
	dir := t.TempDir()
	devDir := filepath.Join(dir, "dev")
	require.NoError(t, os.MkdirAll(devDir, 0755))
	require.NoError(t, os.Symlink("/dev/ublkb999", filepath.Join(devDir, "stale")))
	c, err := config.Parse("stale", []byte("cow:\n  file: "+dir+"/stale.cow\nsegments:\n  - type: zero\n    size: 1M\n"))
	require.NoError(t, err)
	d, err := Start(context.Background(), c, devDir)
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	target, err := os.Readlink(d.Path)
	require.NoError(t, err)
	assert.Equal(t, d.BlockPath, target)
}
