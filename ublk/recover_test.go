package ublk

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	helperFileEnv = "BLKMAP_UBLK_HELPER_FILE"
)

// fileBackend serves a plain file, so data outlives the process serving it.
type fileBackend struct {
	f    *os.File
	size int64
}

func openFileBackend(t testing.TB, path string, size int64) *fileBackend {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(size))
	return &fileBackend{f: f, size: size}
}

func (b *fileBackend) ReadAt(p []byte, off int64) (int, error)  { return b.f.ReadAt(p, off) }
func (b *fileBackend) WriteAt(p []byte, off int64) (int, error) { return b.f.WriteAt(p, off) }
func (b *fileBackend) Size() int64                              { return b.size }
func (b *fileBackend) Flush() error                             { return b.f.Sync() }

func TestCheckRecoverable(t *testing.T) {
	p := &Params{Backend: newMem(4 << 20), Recovery: true}
	p.defaults()
	kp := buildParams(p)
	info := &devInfo{NrHwQueues: 2, QueueDepth: uint16(p.QueueDepth), MaxIOBufBytes: uint32(p.MaxIOSize), Flags: featUserRecovery | featUserRecoveryReissue}
	require.NoError(t, checkRecoverable(info, kp, p))
	// Anything the kernel device was built with that the new server would serve differently
	bad := []func(i *devInfo, k *params){
		func(i *devInfo, k *params) { i.Flags = 0 },
		func(i *devInfo, k *params) { i.QueueDepth /= 2 },
		func(i *devInfo, k *params) { i.MaxIOBufBytes /= 2 },
		func(i *devInfo, k *params) { k.Basic.DevSectors++ },
		func(i *devInfo, k *params) { k.Basic.LogicalBSShift = 12 },
		func(i *devInfo, k *params) { k.Basic.Attrs |= attrReadOnly },
	}
	for n, mutate := range bad {
		i, k := *info, *kp
		mutate(&i, &k)
		assert.Error(t, checkRecoverable(&i, &k, p), "case %d", n)
	}
}

func TestAddDeviceFlags(t *testing.T) {
	assert.Equal(t, uint64(featURingCmdCompInTask), deviceFlags(&Params{}))
	assert.Equal(t, uint64(featURingCmdCompInTask|featUserRecovery|featUserRecoveryReissue), deviceFlags(&Params{Recovery: true}))
}

func TestHelperServeRecoverable(t *testing.T) {
	path := os.Getenv(helperFileEnv)
	if path == "" {
		t.Skip("helper process only")
	}
	d, err := Create(&Params{Backend: openFileBackend(t, path, 4<<20), Recovery: true})
	if err != nil {
		fmt.Println("ERR", err)
		os.Exit(1)
	}
	fmt.Println("ID", d.ID)
	select {}
}

// startRecoverableHelper starts a server process for a recoverable device over path.
func startRecoverableHelper(t *testing.T, path string) (*exec.Cmd, uint32) {
	cmd := exec.Command(os.Args[0], "-test.run", "TestHelperServeRecoverable$")
	cmd.Env = append(os.Environ(), helperFileEnv+"="+path)
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	var id uint32
	_, err = fmt.Fscanf(out, "ID %d\n", &id)
	require.NoError(t, err)
	return cmd, id
}

func TestFeatures(t *testing.T) {
	requireUblk(t)
	features, err := Features()
	require.NoError(t, err)
	assert.NotZero(t, features&featUserRecovery)
	assert.NotZero(t, features&featUserRecoveryReissue)
}

func TestRecoverAfterServerDeath(t *testing.T) {
	requireUblk(t)
	path := filepath.Join(t.TempDir(), "backing")
	cmd, id := startRecoverableHelper(t, path)
	blockPath := fmt.Sprintf("%s%d", blockPrefix, id)
	f, err := os.OpenFile(blockPath, os.O_RDWR|syscall.O_DIRECT, 0)
	require.NoError(t, err)
	defer f.Close()
	before := alignedBuf(4096)
	copy(before, bytes.Repeat([]byte("before"), 600))
	_, err = f.WriteAt(before, 0)
	require.NoError(t, err)
	require.NoError(t, f.Sync())
	// The server dies; the device and the open file survive, I/O waits instead of failing
	require.NoError(t, cmd.Process.Kill())
	cmd.Wait()
	require.Eventually(t, func() bool {
		info, err := GetInfo(id)
		return err == nil && info.Quiesced
	}, 15*time.Second, 50*time.Millisecond)
	waiting := make(chan error, 1)
	go func() {
		p := alignedBuf(4096)
		_, err := f.ReadAt(p, 0)
		if err == nil && !bytes.Equal(p, before) {
			err = fmt.Errorf("read back %q", p[:16])
		}
		waiting <- err
	}()
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-waiting:
		t.Fatalf("I/O completed without a server: %v", err)
	default:
	}
	// A new server re-attaches; the waiting read completes with the data
	d, err := Recover(id, &Params{Backend: openFileBackend(t, path, 4<<20), Recovery: true})
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	select {
	case err := <-waiting:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("I/O issued while the server was gone never completed")
	}
	after := alignedBuf(4096)
	copy(after, bytes.Repeat([]byte("after!"), 600))
	_, err = f.WriteAt(after, 8192)
	require.NoError(t, err)
	info, err := GetInfo(id)
	require.NoError(t, err)
	assert.True(t, info.Live)
	assert.Equal(t, os.Getpid(), info.ServerPID)
	require.NoError(t, f.Close())
	require.NoError(t, d.Close())
}

func TestRecoverRefusesIncompatibleAndPlainDevices(t *testing.T) {
	requireUblk(t)
	path := filepath.Join(t.TempDir(), "backing")
	cmd, id := startRecoverableHelper(t, path)
	require.NoError(t, cmd.Process.Kill())
	cmd.Wait()
	require.Eventually(t, func() bool {
		info, err := GetInfo(id)
		return err == nil && info.Quiesced
	}, 15*time.Second, 50*time.Millisecond)
	// A different size cannot take over the device
	_, err := Recover(id, &Params{Backend: openFileBackend(t, path+"2", 8<<20), Recovery: true})
	assert.Error(t, err)
	require.NoError(t, Delete(id))
	// A device created without recovery cannot be recovered
	d := createTestDevice(t, &Params{Backend: newMem(4 << 20)})
	_, err = Recover(d.ID, &Params{Backend: newMem(4 << 20), Recovery: true})
	assert.Error(t, err)
}
