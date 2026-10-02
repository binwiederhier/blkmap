package device

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"heckel.io/blkmap/config"
	"heckel.io/blkmap/cow"
	"heckel.io/blkmap/source"
	"heckel.io/blkmap/ublk"
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

// computed is a Source that synthesizes its content: block i is filled with byte i. It is
// what a program using blkmap as a library would plug in.
type computed struct {
	size int64
}

func (c *computed) ReadAt(p []byte, off int64) (int, error) {
	for i := range p {
		p[i] = byte((off + int64(i)) / 4096)
	}
	return len(p), nil
}

func (c *computed) Size() int64 {
	return c.size
}

func (c *computed) Close() error {
	return nil
}

func TestServeLibrarySource(t *testing.T) {
	requireUblk(t)
	dir := t.TempDir()
	d, err := Serve(context.Background(), &Options{
		ID:      "lib",
		Base:    &computed{size: 8 << 20},
		COWFile: filepath.Join(dir, "lib.cow"),
		DevDir:  filepath.Join(dir, "dev"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	f, err := os.OpenFile(d.Path, os.O_RDWR, 0)
	require.NoError(t, err)
	t.Cleanup(func() { f.Close() })
	// Reads come from the synthetic source, with 1 MiB requests now that the ublk layer
	// sizes its buffers to the maximum request
	buf := make([]byte, 1<<20)
	_, err = f.ReadAt(buf, 3<<20)
	require.NoError(t, err)
	for i := 0; i < len(buf); i += 4096 {
		require.Equal(t, byte((3<<20+i)/4096), buf[i], "block at %d", 3<<20+i)
	}
	// Writes go to the overlay and read back
	_, err = f.WriteAt(bytes.Repeat([]byte{0xee}, 8192), 5<<20)
	require.NoError(t, err)
	require.NoError(t, f.Sync())
	got := make([]byte, 8192)
	_, err = f.ReadAt(got, 5<<20)
	require.NoError(t, err)
	assert.Equal(t, bytes.Repeat([]byte{0xee}, 8192), got)
	assert.Equal(t, int64(1), d.Written())
	// Defaults were applied
	_, err = os.Stat(filepath.Join(dir, "lib.cow.bitmap"))
	require.NoError(t, err)
}

const (
	helperEnv = "BLKMAP_DEVICE_HELPER"
)

// TestHelperServe is not a test: as a child process it creates a bare ublk device, prints
// its id and sleeps until killed, standing in for a blkmap server that crashed.
func TestHelperServe(t *testing.T) {
	if os.Getenv(helperEnv) == "" {
		t.Skip("helper process only")
	}
	d, err := ublk.Create(&ublk.Params{Backend: &memBackend{data: make([]byte, 1<<20)}})
	if err != nil {
		fmt.Println("ERR", err)
		os.Exit(1)
	}
	fmt.Println("ID", d.ID)
	select {}
}

func TestServeCleansUpDeadPredecessor(t *testing.T) {
	requireUblk(t)
	dir := t.TempDir()
	runDir := filepath.Join(dir, "run")
	// A device whose server died without deleting it
	cmd := exec.Command(os.Args[0], "-test.run", "TestHelperServe$")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	var old uint32
	_, err = fmt.Fscanf(out, "ID %d\n", &old)
	require.NoError(t, err)
	require.NoError(t, cmd.Process.Kill())
	cmd.Wait()
	require.NoError(t, os.MkdirAll(runDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(runDir, "pred"), []byte(fmt.Sprintf("%d\n", old)), 0600))
	d, err := Serve(context.Background(), &Options{
		ID:      "pred",
		Base:    &computed{size: 4 << 20},
		COWFile: filepath.Join(dir, "pred.cow"),
		DevDir:  filepath.Join(dir, "dev"),
		RunDir:  runDir,
	})
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	// The dead predecessor is gone; the kernel may have handed its id to our new device
	if info, err := ublk.GetInfo(old); err == nil {
		assert.Equal(t, os.Getpid(), info.ServerPID, "the old id should now be our new device")
	} else {
		assert.ErrorIs(t, err, syscall.ENODEV)
	}
	// The state file now names the new device and goes away on Close
	state, err := os.ReadFile(filepath.Join(runDir, "pred"))
	require.NoError(t, err)
	assert.Equal(t, strings.TrimPrefix(d.BlockPath, "/dev/ublkb")+"\n", string(state))
	require.NoError(t, d.Close())
	_, err = os.Stat(filepath.Join(runDir, "pred"))
	assert.True(t, os.IsNotExist(err))
}

func TestServeIgnoresBogusStateFile(t *testing.T) {
	requireUblk(t)
	dir := t.TempDir()
	runDir := filepath.Join(dir, "run")
	require.NoError(t, os.MkdirAll(runDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(runDir, "bogus"), []byte("999999\n"), 0600))
	d, err := Serve(context.Background(), &Options{
		ID:      "bogus",
		Base:    &computed{size: 4 << 20},
		COWFile: filepath.Join(dir, "bogus.cow"),
		DevDir:  filepath.Join(dir, "dev"),
		RunDir:  runDir,
	})
	require.NoError(t, err)
	require.NoError(t, d.Close())
}

// memBackend is a minimal ublk backend for the predecessor device.
type memBackend struct {
	data []byte
}

func (m *memBackend) ReadAt(p []byte, off int64) (int, error) {
	return copy(p, m.data[off:]), nil
}

func (m *memBackend) WriteAt(p []byte, off int64) (int, error) {
	return copy(m.data[off:], p), nil
}

func (m *memBackend) Size() int64 {
	return int64(len(m.data))
}

func (m *memBackend) Flush() error {
	return nil
}

func TestServeHydrates(t *testing.T) {
	requireUblk(t)
	dir := t.TempDir()
	var final Progress
	done := make(chan struct{})
	d, err := Serve(context.Background(), &Options{
		ID:      "hyd",
		Base:    &computed{size: 8 << 20},
		COWFile: filepath.Join(dir, "hyd.cow"),
		DevDir:  filepath.Join(dir, "dev"),
		RunDir:  filepath.Join(dir, "run"),
		Hydrate: &Hydrate{
			Prefetch: []source.Range{{Offset: 4 << 20, Length: 1 << 20}},
			Rest:     true,
			Report:   50 * time.Millisecond,
			OnProgress: func(p Progress) {
				if p.Done {
					final = p
					close(done)
				}
			},
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("hydration did not finish")
	}
	assert.Equal(t, int64(128), final.Hydrated) // 8 MiB / 64 KiB
	assert.Equal(t, int64(128), final.Total)
	assert.Equal(t, int64(128), d.Written())
	// Everything reads through the kernel as the synthetic source would have produced it
	f, err := os.Open(d.Path)
	require.NoError(t, err)
	defer f.Close()
	buf := make([]byte, 8<<20)
	_, err = f.ReadAt(buf, 0)
	require.NoError(t, err)
	for i := 0; i < len(buf); i += 4096 {
		require.Equal(t, byte(i/4096), buf[i], "block at %d", i)
	}
}

func TestStartDetachedWhenFullyHydrated(t *testing.T) {
	requireUblk(t)
	dir := t.TempDir()
	img := filepath.Join(dir, "img")
	require.NoError(t, os.WriteFile(img, pattern(1<<20), 0600))
	yml := "cow:\n  file: " + dir + "/det.cow\nsegments:\n  - type: file\n    path: " + img + "\n"
	c, err := config.Parse("det", []byte(yml))
	require.NoError(t, err)
	// Fully hydrate, then remove the source image
	d, err := startWithHydrate(context.Background(), c, filepath.Join(dir, "dev"), filepath.Join(dir, "run"), &Hydrate{Rest: true})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return d.Written() == 16 }, 30*time.Second, 50*time.Millisecond)
	require.NoError(t, d.Close())
	require.NoError(t, os.Remove(img))
	// A fresh start no longer needs the source
	d, err = Start(context.Background(), c, filepath.Join(dir, "dev"))
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	f, err := os.Open(d.Path)
	require.NoError(t, err)
	got := make([]byte, 1<<20)
	_, err = f.ReadAt(got, 0)
	require.NoError(t, err)
	assert.Equal(t, pattern(1<<20), got)
	require.NoError(t, f.Close()) // DEL_DEV waits for openers of the block device
	// Without the bitmap the missing image is an error again
	require.NoError(t, d.Close())
	require.NoError(t, os.Remove(filepath.Join(dir, "det.cow.bitmap")))
	_, err = Start(context.Background(), c, filepath.Join(dir, "dev"))
	require.Error(t, err)
}

func TestCloseFlushesBeforeWaitingForOpeners(t *testing.T) {
	requireUblk(t)
	dir := t.TempDir()
	d, err := Serve(context.Background(), &Options{
		ID:      "flush",
		Base:    &computed{size: 4 << 20},
		COWFile: filepath.Join(dir, "flush.cow"),
		DevDir:  filepath.Join(dir, "dev"),
		RunDir:  filepath.Join(dir, "run"),
	})
	require.NoError(t, err)
	// A direct write reaches the daemon without any flush, so the bitmap bit is only in memory
	f, err := os.OpenFile(d.Path, os.O_RDWR|syscall.O_DIRECT, 0)
	require.NoError(t, err)
	raw := make([]byte, 8192)
	data := raw[4096-int(uintptr(unsafe.Pointer(&raw[0]))%4096):][:4096]
	copy(data, bytes.Repeat([]byte("flushed!"), 512))
	_, err = f.WriteAt(data, 1<<20)
	require.NoError(t, err)
	// Close while f keeps the device open: deletion has to wait, but everything must
	// already be on disk by then
	closed := make(chan error, 1)
	go func() { closed <- d.Close() }()
	time.Sleep(time.Second)
	select {
	case err := <-closed:
		t.Fatalf("Close returned while the device was still open: %v", err)
	default:
	}
	info, err := cow.Inspect(filepath.Join(dir, "flush.cow.bitmap"))
	require.NoError(t, err)
	assert.Equal(t, int64(1), info.Written)
	cowData, err := os.ReadFile(filepath.Join(dir, "flush.cow"))
	require.NoError(t, err)
	assert.Equal(t, data, cowData[1<<20:1<<20+len(data)])
	require.NoError(t, f.Close())
	require.NoError(t, <-closed)
	_, err = os.Lstat(d.Path)
	assert.True(t, os.IsNotExist(err))
}

func TestPeriodicFlush(t *testing.T) {
	requireUblk(t)
	dir := t.TempDir()
	d, err := Serve(context.Background(), &Options{
		ID:      "tick",
		Base:    &computed{size: 4 << 20},
		COWFile: filepath.Join(dir, "tick.cow"),
		DevDir:  filepath.Join(dir, "dev"),
		RunDir:  filepath.Join(dir, "run"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	f, err := os.OpenFile(d.Path, os.O_RDWR|syscall.O_DIRECT, 0)
	require.NoError(t, err)
	buf := make([]byte, 8192)
	_, err = f.WriteAt(buf[4096-int(uintptr(unsafe.Pointer(&buf[0]))%4096):][:4096], 2<<20)
	require.NoError(t, err)
	require.NoError(t, f.Close()) // no fsync anywhere
	require.Eventually(t, func() bool {
		info, err := cow.Inspect(filepath.Join(dir, "tick.cow.bitmap"))
		return err == nil && info.Written == 1
	}, 3*flushInterval, 200*time.Millisecond, "the bit should reach disk without a guest flush")
}

func TestCloseLeavesSuccessorAlone(t *testing.T) {
	requireUblk(t)
	dir := t.TempDir()
	opts := func() *Options {
		return &Options{ID: "succ", Base: &computed{size: 4 << 20}, COWFile: filepath.Join(dir, "succ.cow"), DevDir: filepath.Join(dir, "dev"), RunDir: filepath.Join(dir, "run")}
	}
	old, err := Serve(context.Background(), opts())
	require.NoError(t, err)
	// A restart races the old server's Close: the new server has already published its
	// symlink and state file under the same id when the old one gets to its cleanup
	require.NoError(t, old.ublk.Stop())
	o := opts()
	o.COWFile = filepath.Join(dir, "succ2.cow")
	next, err := Serve(context.Background(), o)
	require.NoError(t, err)
	t.Cleanup(func() { next.Close() })
	require.NoError(t, old.Close())
	target, err := os.Readlink(next.Path)
	require.NoError(t, err, "the successor's symlink must survive the predecessor's Close")
	assert.Equal(t, next.BlockPath, target)
	state, err := os.ReadFile(filepath.Join(dir, "run", "succ"))
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("%d\n", next.ublk.ID), string(state))
}
