package ublk

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// mem is an in-memory Backend that records flushes and discards and can fail a range.
type mem struct {
	data     []byte
	flushes  int
	discards [][2]int64
	zeroes   [][2]int64
	failFrom int64 // reads at or past this offset fail when > 0
	mu       sync.Mutex
}

func newMem(size int) *mem {
	return &mem{data: make([]byte, size)}
}

func (m *mem) ReadAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failFrom > 0 && off+int64(len(p)) > m.failFrom {
		return 0, errors.New("injected read failure")
	}
	return copy(p, m.data[off:]), nil
}

func (m *mem) WriteAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return copy(m.data[off:], p), nil
}

func (m *mem) Size() int64 {
	return int64(len(m.data))
}

func (m *mem) Flush() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flushes++
	return nil
}

func (m *mem) Discard(off, length int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.discards = append(m.discards, [2]int64{off, length})
	clear(m.data[off : off+length])
	return nil
}

func (m *mem) WriteZeroes(off, length int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.zeroes = append(m.zeroes, [2]int64{off, length})
	clear(m.data[off : off+length])
	return nil
}

// plain hides the optional interfaces so the device advertises neither discard nor zeroes.
type plain struct {
	m *mem
}

func (p *plain) ReadAt(b []byte, off int64) (int, error) {
	return p.m.ReadAt(b, off)
}

func (p *plain) WriteAt(b []byte, off int64) (int, error) {
	return p.m.WriteAt(b, off)
}

func (p *plain) Size() int64 {
	return p.m.Size()
}

func (p *plain) Flush() error {
	return p.m.Flush()
}

func requireUblk(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if _, err := os.Stat(controlPath); err != nil {
		t.Skip("needs ublk_drv loaded")
	}
}

func pattern(n int, seed byte) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(i*7+i/256) ^ seed
	}
	return p
}

// alignedBuf returns a page-aligned buffer, as O_DIRECT requires.
func alignedBuf(n int) []byte {
	b := make([]byte, n+4096)
	off := 4096 - int(uintptr(unsafePointer(b))%4096)
	return b[off : off+n]
}

func createTestDevice(t *testing.T, p *Params) *Device {
	t.Helper()
	d, err := Create(p)
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	return d
}

func TestParamsValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		params *Params
		errMsg string
	}{
		{"nil backend", &Params{}, "backend"},
		{"bad block size", &Params{Backend: newMem(1 << 20), BlockSize: 1024}, "block size"},
		{"size not aligned", &Params{Backend: newMem(1000)}, "multiple of the block size"},
		{"max io too small", &Params{Backend: newMem(1 << 20), MaxIOSize: 512}, "max I/O size"},
		{"max io not aligned", &Params{Backend: newMem(1 << 20), MaxIOSize: 4096 + 512, BlockSize: 4096}, "max I/O size"},
		{"depth too large", &Params{Backend: newMem(1 << 20), QueueDepth: maxQueueDepth + 1}, "queue depth"},
		{"empty backend", &Params{Backend: newMem(0)}, "size"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Create(tt.params)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errMsg)
		})
	}
}

func TestBuildParams(t *testing.T) {
	t.Parallel()
	p := &Params{Backend: newMem(64 << 20), BlockSize: 4096, MaxIOSize: 1 << 20, ReadOnly: true}
	p.defaults()
	kp := buildParams(p)
	assert.Equal(t, uint32(paramTypeBasic|paramTypeDiscard), kp.Types)
	assert.Equal(t, uint8(12), kp.Basic.LogicalBSShift)
	assert.Equal(t, uint8(12), kp.Basic.PhysicalBSShift)
	assert.Equal(t, uint32(attrReadOnly|attrVolatileCache), kp.Basic.Attrs)
	assert.Equal(t, uint32(2048), kp.Basic.MaxSectors)   // 1 MiB in 512-byte sectors
	assert.Equal(t, uint64(131072), kp.Basic.DevSectors) // 64 MiB in sectors
	assert.Equal(t, uint32(4096), kp.Discard.DiscardGranularity)
	assert.Equal(t, uint16(1), kp.Discard.MaxDiscardSegments)
	assert.NotZero(t, kp.Discard.MaxDiscardSectors)
	assert.NotZero(t, kp.Discard.MaxWriteZeroesSectors)
	// Without the optional interfaces nothing is advertised
	p = &Params{Backend: &plain{m: newMem(64 << 20)}}
	p.defaults()
	kp = buildParams(p)
	assert.Equal(t, uint32(paramTypeBasic), kp.Types)
	assert.Equal(t, uint8(9), kp.Basic.LogicalBSShift)
}

func TestDeviceLifecycle(t *testing.T) {
	requireUblk(t)
	m := newMem(64 << 20)
	d := createTestDevice(t, &Params{Backend: m})
	assert.Regexp(t, `^/dev/ublkb\d+$`, d.BlockPath)
	f, err := os.OpenFile(d.BlockPath, os.O_RDWR|syscall.O_DIRECT, 0)
	require.NoError(t, err)
	t.Cleanup(func() { f.Close() })
	size, err := f.Seek(0, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(64<<20), size)
	// Write the whole device with 1 MiB requests (the maximum), then read it back with
	// every request size from one sector to 4 MiB, direct and buffered
	want := pattern(64<<20, 1)
	buf := alignedBuf(1 << 20)
	for off := 0; off < len(want); off += len(buf) {
		copy(buf, want[off:])
		n, err := f.WriteAt(buf, int64(off))
		require.NoError(t, err)
		require.Equal(t, len(buf), n)
	}
	require.NoError(t, f.Sync())
	assert.Equal(t, want, m.data)
	for _, bs := range []int{512, 4096, 64 << 10, 1 << 20, 4 << 20} {
		got := alignedBuf(len(want))
		for off := 0; off < len(want); off += bs {
			n, err := f.ReadAt(got[off:off+bs], int64(off))
			require.NoError(t, err, "bs %d off %d", bs, off)
			require.Equal(t, bs, n)
		}
		require.True(t, bytes.Equal(want, got), "direct read with bs %d differs", bs)
	}
	m.mu.Lock()
	flushes := m.flushes
	m.mu.Unlock()
	assert.Greater(t, flushes, 0)
	// Buffered path too
	bf, err := os.Open(d.BlockPath)
	require.NoError(t, err)
	got := make([]byte, len(want))
	_, err = bf.ReadAt(got, 0)
	require.NoError(t, err)
	bf.Close()
	assert.True(t, bytes.Equal(want, got), "buffered read differs")
	// Close tears the device down
	require.NoError(t, f.Close())
	require.NoError(t, d.Close())
	_, err = os.Stat(d.BlockPath)
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(d.CharPath)
	assert.True(t, os.IsNotExist(err))
	assert.NoError(t, d.Close()) // idempotent
}

func TestDeviceConcurrent(t *testing.T) {
	requireUblk(t)
	m := newMem(32 << 20)
	d := createTestDevice(t, &Params{Backend: m, NumQueues: 4, QueueDepth: 32})
	// 16 writers each own a 2 MiB region and hammer it with random-size direct I/O
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			f, err := os.OpenFile(d.BlockPath, os.O_RDWR|syscall.O_DIRECT, 0)
			if !assert.NoError(t, err) {
				return
			}
			defer f.Close()
			base := int64(w) * (2 << 20)
			rng := rand.New(rand.NewSource(int64(w)))
			shadow := make([]byte, 2<<20)
			for i := 0; i < 200; i++ {
				n := 4096 * (1 + rng.Intn(64))
				off := int64(rng.Intn((2<<20-n)/4096+1)) * 4096
				data := alignedBuf(n)
				rng.Read(data)
				copy(shadow[off:], data)
				if _, err := f.WriteAt(data, base+off); !assert.NoError(t, err) {
					return
				}
				got := alignedBuf(n)
				if _, err := f.ReadAt(got, base+off); !assert.NoError(t, err) {
					return
				}
				assert.True(t, bytes.Equal(data, got), "writer %d iteration %d", w, i)
			}
			full := alignedBuf(2 << 20)
			_, err = f.ReadAt(full, base)
			assert.NoError(t, err)
			assert.True(t, bytes.Equal(shadow, full), "writer %d final region", w)
		}(w)
	}
	wg.Wait()
}

func TestDeviceReadOnly(t *testing.T) {
	requireUblk(t)
	d := createTestDevice(t, &Params{Backend: newMem(8 << 20), ReadOnly: true})
	f, err := os.OpenFile(d.BlockPath, os.O_RDWR, 0)
	require.NoError(t, err)
	defer f.Close()
	ro, err := unix.IoctlGetInt(int(f.Fd()), unix.BLKROGET)
	require.NoError(t, err)
	assert.Equal(t, 1, ro)
	_, err = f.WriteAt(make([]byte, 4096), 0)
	require.Error(t, err)
}

func TestDeviceDiscardAndZeroes(t *testing.T) {
	requireUblk(t)
	m := newMem(8 << 20)
	copy(m.data, pattern(8<<20, 2))
	d := createTestDevice(t, &Params{Backend: m})
	f, err := os.OpenFile(d.BlockPath, os.O_RDWR, 0)
	require.NoError(t, err)
	defer f.Close()
	// BLKDISCARD and BLKZEROOUT take a [start, length] pair of uint64
	require.NoError(t, blkIoctl(f, unix.BLKDISCARD, 1<<20, 2<<20))
	require.NoError(t, blkIoctl(f, unix.BLKZEROOUT, 4<<20, 1<<20))
	require.NoError(t, f.Sync())
	m.mu.Lock()
	defer m.mu.Unlock()
	var discarded int64
	for _, r := range m.discards {
		assert.GreaterOrEqual(t, r[0], int64(1<<20))
		assert.LessOrEqual(t, r[0]+r[1], int64(3<<20))
		discarded += r[1]
	}
	assert.Equal(t, int64(2<<20), discarded)
	var zeroed int64
	for _, r := range m.zeroes {
		assert.GreaterOrEqual(t, r[0], int64(4<<20))
		assert.LessOrEqual(t, r[0]+r[1], int64(5<<20))
		zeroed += r[1]
	}
	assert.Equal(t, int64(1<<20), zeroed)
	assert.Equal(t, make([]byte, 2<<20), m.data[1<<20:3<<20])
	assert.Equal(t, make([]byte, 1<<20), m.data[4<<20:5<<20])
	assert.Equal(t, pattern(8<<20, 2)[:1<<20], m.data[:1<<20])
}

// blkIoctl issues a block-device range ioctl ([start, length] uint64 pair).
func blkIoctl(f *os.File, req uint, start, length uint64) error {
	rng := [2]uint64{start, length}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), uintptr(req), uintptr(unsafe.Pointer(&rng[0])))
	if errno != 0 {
		return errno
	}
	return nil
}

func TestDeviceBackendError(t *testing.T) {
	requireUblk(t)
	m := newMem(8 << 20)
	m.failFrom = 4 << 20
	d := createTestDevice(t, &Params{Backend: m})
	f, err := os.OpenFile(d.BlockPath, os.O_RDONLY|syscall.O_DIRECT, 0)
	require.NoError(t, err)
	defer f.Close()
	buf := alignedBuf(4096)
	_, err = f.ReadAt(buf, 0)
	require.NoError(t, err)
	_, err = f.ReadAt(buf, 5<<20)
	require.Error(t, err)
	assert.ErrorIs(t, err, syscall.EIO)
	// The device keeps working after an error
	_, err = f.ReadAt(buf, 1<<20)
	require.NoError(t, err)
}

func TestTwoDevices(t *testing.T) {
	requireUblk(t)
	a := createTestDevice(t, &Params{Backend: newMem(4 << 20)})
	b := createTestDevice(t, &Params{Backend: newMem(4 << 20)})
	assert.NotEqual(t, a.ID, b.ID)
	for _, d := range []*Device{a, b} {
		f, err := os.OpenFile(d.BlockPath, os.O_RDWR, 0)
		require.NoError(t, err)
		_, err = f.WriteAt([]byte("hello"), 0)
		require.NoError(t, err)
		require.NoError(t, f.Sync())
		f.Close()
	}
	require.NoError(t, a.Close())
	// b still serves after a is gone
	f, err := os.Open(b.BlockPath)
	require.NoError(t, err)
	buf := make([]byte, 5)
	_, err = f.ReadAt(buf, 0)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(buf))
	f.Close()
}

const (
	helperEnv = "BLKMAP_UBLK_HELPER"
)

// TestHelperServe is not a test: run as a child process it creates a device, prints its id
// and sleeps until killed, to simulate a server that dies without cleaning up.
func TestHelperServe(t *testing.T) {
	if os.Getenv(helperEnv) == "" {
		t.Skip("helper process only")
	}
	d, err := Create(&Params{Backend: newMem(4 << 20)})
	if err != nil {
		fmt.Println("ERR", err)
		os.Exit(1)
	}
	fmt.Println("ID", d.ID)
	select {}
}

func TestDeleteAfterServerDeath(t *testing.T) {
	requireUblk(t)
	cmd := exec.Command(os.Args[0], "-test.run", "TestHelperServe$")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	var id uint32
	_, err = fmt.Fscanf(out, "ID %d\n", &id)
	require.NoError(t, err)
	info, err := GetInfo(id)
	require.NoError(t, err)
	assert.True(t, info.Live)
	assert.Equal(t, cmd.Process.Pid, info.ServerPID)
	require.NoError(t, cmd.Process.Kill())
	cmd.Wait()
	// The kernel keeps the device around without a server; Delete cleans it up
	require.Eventually(t, func() bool {
		info, err := GetInfo(id)
		return err == nil && !info.Live
	}, 10*time.Second, 100*time.Millisecond)
	require.NoError(t, Delete(id))
	_, err = GetInfo(id)
	assert.ErrorIs(t, err, syscall.ENODEV)
	assert.ErrorIs(t, Delete(id), syscall.ENODEV)
	_, err = os.Stat(fmt.Sprintf("%s%d", charPrefix, id))
	assert.True(t, os.IsNotExist(err))
}

func TestDeviceStopThenDelete(t *testing.T) {
	requireUblk(t)
	m := newMem(4 << 20)
	d := createTestDevice(t, &Params{Backend: m})
	f, err := os.OpenFile(d.BlockPath, os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte("stop"), 0)
	require.NoError(t, err)
	require.NoError(t, f.Sync())
	require.NoError(t, f.Close())
	require.NoError(t, d.Stop())
	assert.Equal(t, "stop", string(m.data[:4]))
	_, err = os.Stat(d.BlockPath)
	assert.True(t, os.IsNotExist(err), "the block device goes away on Stop")
	info, err := GetInfo(d.ID)
	require.NoError(t, err)
	assert.False(t, info.Live)
	require.NoError(t, d.Stop()) // idempotent
	require.NoError(t, d.Delete())
	_, err = GetInfo(d.ID)
	assert.ErrorIs(t, err, syscall.ENODEV)
	require.NoError(t, d.Delete())
	require.NoError(t, d.Close())
}
