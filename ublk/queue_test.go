package ublk

import (
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestObserveLatchesParallelMode(t *testing.T) {
	q := &queue{}
	for i := 0; i < 20; i++ {
		q.observe(2 * time.Millisecond)
	}
	assert.True(t, q.parallel, "slow reads switch to the workers")
	// Cache hits interleaved with slow reads must not flip the queue back to inline, where
	// the next slow read would block every request behind it
	for i := 0; i < 50; i++ {
		q.observe(5 * time.Microsecond)
	}
	q.observe(2 * time.Millisecond)
	for i := 0; i < 50; i++ {
		q.observe(5 * time.Microsecond)
	}
	assert.True(t, q.parallel, "a slow read within the window keeps the workers")
	for i := 0; i < inlineAfter; i++ {
		q.observe(5 * time.Microsecond)
	}
	assert.False(t, q.parallel, "a long run of fast reads returns to inline")
}

// fakeQueue builds a queue over a fake descriptor array, enough for serve to run without a
// kernel device.
func fakeQueue(backend Backend, depth int) *queue {
	p := &Params{Backend: backend}
	p.defaults()
	p.MaxIOSize = 1 << 16
	return &queue{
		dev:   &Device{params: p},
		descs: make([]byte, depth*int(unsafe.Sizeof(ioDesc{}))),
		bufs:  make([]byte, depth*p.MaxIOSize),
	}
}

func (q *queue) setDesc(tag uint16, op uint32, startSector uint64, nrSectors uint32) {
	d := (*ioDesc)(unsafe.Pointer(&q.descs[int(tag)*int(unsafe.Sizeof(ioDesc{}))]))
	d.OpFlags, d.StartSector, d.NrSectors = op, startSector, nrSectors
}

func TestOldestInflight(t *testing.T) {
	q := fakeQueue(newMem(1<<20), 4)
	q.started, q.lastCmd = make([]int64, 4), make([]uint32, 4)
	now := time.Now()
	assert.Zero(t, q.oldestInflight(now.UnixNano()), "nothing dispatched")
	q.dispatch(1)
	q.dispatch(3)
	q.started[3] = now.Add(-3 * time.Second).UnixNano()
	assert.InDelta(t, 3*time.Second, q.oldestInflight(now.UnixNano()), float64(100*time.Millisecond))
	q.finish(3)
	assert.Less(t, q.oldestInflight(now.Add(time.Second).UnixNano()), 2*time.Second, "the younger one remains")
	q.finish(1)
	assert.Zero(t, q.oldestInflight(now.UnixNano()))
}

// A refused fetch only costs the tag; a refused commit leaves a request nobody can complete
// and fails the device, except when the kernel is cancelling the queue itself.
func TestCommandFailedFailsTheDeviceOnALostCommit(t *testing.T) {
	q := fakeQueue(newMem(1<<20), 4)
	q.lastCmd = make([]uint32, 4)
	q.dev.failed = make(chan struct{})
	q.lastCmd[0] = cmdFetchReq
	q.commandFailed(0, syscall.EINVAL)
	assert.NoError(t, q.dev.Err(), "a refused fetch has no request behind it")
	q.lastCmd[1] = cmdCommitAndFetchReq
	q.commandFailed(1, syscall.EIO)
	assert.NoError(t, q.dev.Err(), "EIO: the kernel is cancelling the queue and fails the request itself")
	q.commandFailed(1, syscall.EINVAL)
	require.Error(t, q.dev.Err())
	assert.ErrorIs(t, q.dev.Err(), syscall.EINVAL)
	assert.ErrorIs(t, q.dev.Err(), ErrRequestLost, "the owner must know not to STOP_DEV it")
	select {
	case <-q.dev.Done():
	default:
		t.Fatal("Done is not closed")
	}
}

func TestServeRejectsRequestsOutsideTheDevice(t *testing.T) {
	m := newMem(1 << 20)
	q := fakeQueue(m, 4)
	q.setDesc(0, opRead, 1<<20/sectorSize-1, 2) // one sector past the end
	assert.Equal(t, int32(resultEIO), q.serve(0))
	q.setDesc(1, opWrite, 1<<40, 1)
	assert.Equal(t, int32(resultEIO), q.serve(1))
	q.setDesc(2, opRead, 0, 1<<16/sectorSize+1) // bigger than the request buffer
	assert.Equal(t, int32(resultEIO), q.serve(2))
	q.setDesc(3, opRead, 8, 8)
	assert.Equal(t, int32(8*sectorSize), q.serve(3))
	q.setDesc(3, opFlush, ^uint64(0), 0) // a flush has no range: the kernel passes sector -1
	assert.Equal(t, int32(0), q.serve(3))
	assert.Equal(t, 1, m.flushes)
}

func TestDeviceStats(t *testing.T) {
	slow, fast := &queue{}, &queue{}
	for i := 0; i < 20; i++ {
		slow.observe(2 * time.Millisecond)
		fast.observe(5 * time.Microsecond)
	}
	d := &Device{queues: []*queue{slow, fast}}
	assert.Equal(t, Stats{Queues: 2, Parallel: 1}, d.Stats())
}

// slowWrites is a backend whose writes wait for a slow base read, as a first partial write to
// a network-backed store does.
type slowWrites struct{ *mem }

func (b slowWrites) WriteAt(p []byte, off int64) (int, error) {
	time.Sleep(time.Millisecond)
	return b.mem.WriteAt(p, off)
}

func TestSlowWritesActivateParallelMode(t *testing.T) {
	q := fakeQueue(slowWrites{newMem(1 << 20)}, 1)
	for range 2 { // from the start, and again after fast requests returned the queue to inline
		for range 20 {
			q.setDesc(0, opWrite, 8, 8)
			require.Equal(t, int32(8*sectorSize), q.serveInline(0))
		}
		assert.True(t, q.parallel, "slow writes alone switch to the workers")
		for range inlineAfter {
			q.served(0, 5*time.Microsecond)
		}
		require.False(t, q.parallel)
	}
}
