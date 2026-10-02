package device

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"heckel.io/blkmap/cow"
	"heckel.io/blkmap/source"
	"heckel.io/blkmap/ublk"
)

const (
	recoverHelperEnv = "BLKMAP_DEVICE_RECOVER_HELPER" // the test directory
	recoverSize      = 8 << 20
	recoverReexecEnv = "BLKMAP_DEVICE_RECOVER_REEXEC"
)

// recoverOptions are the options both the helper and the test serve the device with.
func recoverOptions(t testing.TB, dir string, size int64) *Options {
	base := filepath.Join(dir, fmt.Sprintf("base-%d", size))
	if _, err := os.Stat(base); err != nil {
		require.NoError(t, os.WriteFile(base, pattern(int(size)), 0600))
	}
	src, err := source.OpenFile(base, 0, 0)
	require.NoError(t, err)
	return &Options{
		ID:       "rec",
		Base:     src,
		COWFile:  filepath.Join(dir, fmt.Sprintf("rec-%d.cow", size)),
		DevDir:   filepath.Join(dir, "dev"),
		RunDir:   filepath.Join(dir, "run"),
		Recovery: true,
	}
}

// TestHelperServeRecoverable is the server process the recovery tests kill or hand off.
func TestHelperServeRecoverable(t *testing.T) {
	dir := os.Getenv(recoverHelperEnv)
	if dir == "" {
		t.Skip("helper process only")
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	d, err := Serve(context.Background(), recoverOptions(t, dir, recoverSize))
	if err != nil {
		fmt.Println("ERR", err)
		os.Exit(1)
	}
	fmt.Println("DEV", d.BlockPath)
	<-hup
	if err := d.Detach(); err != nil {
		os.Exit(1)
	}
	if os.Getenv(recoverReexecEnv) != "" {
		// What blkmap serve does on SIGHUP: the same process, a fresh image
		syscall.Exec(os.Args[0], os.Args, os.Environ())
	}
	os.Exit(ExitDetached)
}

func startRecoverHelper(t *testing.T, dir string) (*exec.Cmd, string) {
	cmd := exec.Command(os.Args[0], "-test.run", "TestHelperServeRecoverable$")
	cmd.Env = append(os.Environ(), recoverHelperEnv+"="+dir)
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	var blockPath string
	_, err = fmt.Fscanf(out, "DEV %s\n", &blockPath)
	require.NoError(t, err)
	return cmd, blockPath
}

// writeUnflushed writes a block through the device without a flush: acknowledged, but only
// in the cow file's page cache and the live bitmap.
func writeUnflushed(t *testing.T, f *os.File, off int64, fill byte) []byte {
	p := alignedBuf(4096)
	copy(p, bytes.Repeat([]byte{fill}, 4096))
	_, err := f.WriteAt(p, off)
	require.NoError(t, err)
	return append([]byte(nil), p...)
}

func readBlock(t *testing.T, f *os.File, off int64) []byte {
	p := alignedBuf(4096)
	_, err := f.ReadAt(p, off)
	require.NoError(t, err)
	return append([]byte(nil), p...)
}

func TestServeRecoversAfterCrash(t *testing.T) {
	requireUblk(t)
	dir := t.TempDir()
	cmd, blockPath := startRecoverHelper(t, dir)
	f, err := os.OpenFile(blockPath, os.O_RDWR|syscall.O_DIRECT, 0)
	require.NoError(t, err)
	defer f.Close()
	written := writeUnflushed(t, f, 64<<10, 'x')
	require.NoError(t, cmd.Process.Kill())
	cmd.Wait()
	// The successor re-attaches to the same kernel device; the open file never noticed
	d, err := Serve(context.Background(), recoverOptions(t, dir, recoverSize))
	require.NoError(t, err)
	assert.Equal(t, blockPath, d.BlockPath)
	assert.Equal(t, written, readBlock(t, f, 64<<10), "an acknowledged write must survive the crash")
	assert.Equal(t, pattern(recoverSize)[:4096], readBlock(t, f, 0))
	writeUnflushed(t, f, 128<<10, 'y')
	require.NoError(t, f.Close())
	require.NoError(t, d.Close())
	info, err := cow.Inspect(filepath.Join(dir, fmt.Sprintf("rec-%d.cow", recoverSize)) + ".bitmap")
	require.NoError(t, err)
	assert.Equal(t, int64(2), info.Written)
	_, err = os.Stat(filepath.Join(dir, "run", "rec"+liveBitmapExt))
	assert.True(t, os.IsNotExist(err), "a clean close removes the live bitmap")
}

func TestServeHandoffOnDetach(t *testing.T) {
	requireUblk(t)
	dir := t.TempDir()
	cmd, blockPath := startRecoverHelper(t, dir)
	f, err := os.OpenFile(blockPath, os.O_RDWR|syscall.O_DIRECT, 0)
	require.NoError(t, err)
	defer f.Close()
	written := writeUnflushed(t, f, 0, 'h')
	require.NoError(t, cmd.Process.Signal(syscall.SIGHUP))
	err = cmd.Wait()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	assert.Equal(t, ExitDetached, exit.ExitCode())
	d, err := Serve(context.Background(), recoverOptions(t, dir, recoverSize))
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	assert.Equal(t, blockPath, d.BlockPath)
	assert.Equal(t, written, readBlock(t, f, 0))
	require.NoError(t, f.Close())
	require.NoError(t, d.Close())
}

func TestServeReplacesUnrecoverablePredecessor(t *testing.T) {
	requireUblk(t)
	dir := t.TempDir()
	cmd, blockPath := startRecoverHelper(t, dir)
	f, err := os.OpenFile(blockPath, os.O_RDWR|syscall.O_DIRECT, 0)
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, cmd.Process.Kill())
	cmd.Wait()
	// The config changed (another size): the waiting device cannot be taken over, so it is
	// deleted (its I/O fails instead of hanging forever) and a fresh device is created
	d, err := Serve(context.Background(), recoverOptions(t, dir, recoverSize/2))
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	assert.Equal(t, int64(recoverSize/2), d.Size())
	_, err = f.ReadAt(alignedBuf(4096), 0)
	assert.Error(t, err, "the old device must fail, not hang")
	require.NoError(t, f.Close())
	require.NoError(t, d.Close())
}

func TestServeRefusesLiveServer(t *testing.T) {
	requireUblk(t)
	dir := t.TempDir()
	cmd, blockPath := startRecoverHelper(t, dir)
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		var id uint32
		fmt.Sscanf(blockPath, "/dev/ublkb%d", &id)
		ublk.Delete(id)
	})
	o := recoverOptions(t, dir, recoverSize)
	o.COWFile = filepath.Join(dir, "other.cow")
	_, err := Serve(context.Background(), o)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already served")
	_, err = os.Stat(blockPath)
	assert.NoError(t, err, "the live device is left alone")
	time.Sleep(10 * time.Millisecond)
}

func TestReapDeletesAbandonedDevice(t *testing.T) {
	requireUblk(t)
	dir := t.TempDir()
	cmd, blockPath := startRecoverHelper(t, dir)
	f, err := os.OpenFile(blockPath, os.O_RDWR|syscall.O_DIRECT, 0)
	require.NoError(t, err)
	defer f.Close()
	// While its server lives, reap leaves the device alone
	require.Error(t, Reap("rec", filepath.Join(dir, "run")))
	require.NoError(t, cmd.Process.Kill())
	cmd.Wait()
	// Nobody will recover it (systemd gave up): reap fails the waiting I/O instead of
	// leaving it hanging, even while the device is still open (a mount)
	require.NoError(t, Reap("rec", filepath.Join(dir, "run")))
	_, err = f.ReadAt(alignedBuf(4096), 0)
	assert.Error(t, err)
	// Once nothing has it open, the next reap deletes the kernel device
	require.NoError(t, f.Close())
	require.NoError(t, Reap("rec", filepath.Join(dir, "run")))
	_, err = os.Stat(filepath.Join(dir, "run", "rec"))
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(strings.Replace(blockPath, "ublkb", "ublkc", 1))
	assert.True(t, os.IsNotExist(err), "the kernel device is gone")
	require.NoError(t, Reap("rec", filepath.Join(dir, "run")), "nothing left to reap is fine")
}

func TestServeReexecHandoff(t *testing.T) {
	requireUblk(t)
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run", "TestHelperServeRecoverable$")
	cmd.Env = append(os.Environ(), recoverHelperEnv+"="+dir, recoverReexecEnv+"=1")
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	var blockPath, again string
	_, err = fmt.Fscanf(out, "DEV %s\n", &blockPath)
	require.NoError(t, err)
	f, err := os.OpenFile(blockPath, os.O_RDWR|syscall.O_DIRECT, 0)
	require.NoError(t, err)
	defer f.Close()
	written := writeUnflushed(t, f, 0, 'e')
	// The server re-executes itself: same pid, so it must recognize the device it served
	// before the exec as its own predecessor rather than a live server to refuse
	require.NoError(t, cmd.Process.Signal(syscall.SIGHUP))
	_, err = fmt.Fscanf(out, "DEV %s\n", &again)
	require.NoError(t, err, "the re-executed server did not come up")
	assert.Equal(t, blockPath, again)
	assert.Equal(t, written, readBlock(t, f, 0))
	require.NoError(t, f.Close())
	require.NoError(t, cmd.Process.Signal(syscall.SIGKILL))
	cmd.Wait()
	var id uint32
	fmt.Sscanf(blockPath, "/dev/ublkb%d", &id)
	ublk.Delete(id)
}
