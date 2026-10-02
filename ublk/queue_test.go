package ublk

import (
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/assert"
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
