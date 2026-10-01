package ublk

import (
	"errors"
	"fmt"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

var (
	errRingFull = errors.New("submission queue full")
)

// ring is a minimal io_uring that only ever submits URING_CMD SQEs to one target fd. The
// struct is always heap allocated, so the timespec and getevents argument it embeds have
// stable addresses the kernel can be handed.
type ring struct {
	fd      int
	target  int32
	entries uint32
	sqHead  *uint32
	sqTail  *uint32
	cqHead  *uint32
	cqTail  *uint32
	sqMask  uint32
	cqMask  uint32
	sqes    unsafe.Pointer
	cqes    unsafe.Pointer
	local   uint32 // SQEs prepared but not yet published to the kernel
	maps    [][]byte
	ts      syscall.Timespec
	arg     getEventsArg
}

// newRing creates a ring with the given number of SQEs targeting fd.
func newRing(entries uint32, target int) (*ring, error) {
	p := &ringParams{Flags: ringSetupSQE128 | ringSetupCQE32}
	fd, _, errno := syscall.Syscall(sysIoUringSetup, uintptr(entries), uintptr(unsafe.Pointer(p)), 0)
	if errno != 0 {
		return nil, fmt.Errorf("io_uring_setup: %w", errno)
	}
	r := &ring{fd: int(fd), target: int32(target), entries: p.SQEntries}
	sq, err := r.mmap(ringOffSQRing, int(p.SQOff.Array+p.SQEntries*4))
	if err != nil {
		return nil, err
	}
	cq, err := r.mmap(ringOffCQRing, int(p.CQOff.CQEs+p.CQEntries*cqeSize))
	if err != nil {
		return nil, err
	}
	sqes, err := r.mmap(ringOffSQEs, int(p.SQEntries)*sqeSize)
	if err != nil {
		return nil, err
	}
	r.sqHead = (*uint32)(unsafe.Add(sq, p.SQOff.Head))
	r.sqTail = (*uint32)(unsafe.Add(sq, p.SQOff.Tail))
	r.sqMask = *(*uint32)(unsafe.Add(sq, p.SQOff.RingMask))
	r.cqHead = (*uint32)(unsafe.Add(cq, p.CQOff.Head))
	r.cqTail = (*uint32)(unsafe.Add(cq, p.CQOff.Tail))
	r.cqMask = *(*uint32)(unsafe.Add(cq, p.CQOff.RingMask))
	r.cqes = unsafe.Add(cq, p.CQOff.CQEs)
	r.sqes = sqes
	r.local = atomic.LoadUint32(r.sqTail)
	// The SQ index array is identity mapped once: slot i always refers to SQE i
	array := unsafe.Add(sq, p.SQOff.Array)
	for i := uint32(0); i < p.SQEntries; i++ {
		*(*uint32)(unsafe.Add(array, uintptr(i)*4)) = i
	}
	r.arg.TS = uint64(uintptr(unsafe.Pointer(&r.ts)))
	return r, nil
}

// prepare writes one URING_CMD SQE with the given cmd payload; it reaches the kernel on flush.
func (r *ring) prepare(cmdOp uint32, userData uint64, cmd unsafe.Pointer, cmdLen uintptr) error {
	if r.local-atomic.LoadUint32(r.sqHead) >= r.entries {
		return errRingFull
	}
	sqe := unsafe.Add(r.sqes, uintptr(r.local&r.sqMask)*sqeSize)
	clear(unsafe.Slice((*byte)(sqe), sqeSize))
	*(*uint8)(sqe) = ringOpURingCmd                  // opcode
	*(*int32)(unsafe.Add(sqe, 4)) = r.target         // fd
	*(*uint32)(unsafe.Add(sqe, 8)) = cmdOp           // cmd_op
	*(*uint32)(unsafe.Add(sqe, 24)) = uint32(cmdLen) // len
	*(*uint64)(unsafe.Add(sqe, 32)) = userData
	copy(unsafe.Slice((*byte)(unsafe.Add(sqe, sqeCmdOffset)), cmdLen), unsafe.Slice((*byte)(cmd), cmdLen))
	r.local++
	return nil
}

// flush publishes the prepared SQEs and submits them with one io_uring_enter.
func (r *ring) flush() error {
	pending := r.local - atomic.LoadUint32(r.sqTail)
	if pending == 0 {
		return nil
	}
	atomic.StoreUint32(r.sqTail, r.local) // the atomic store is the release fence the kernel needs
	for {
		_, _, errno := syscall.Syscall6(sysIoUringEnter, uintptr(r.fd), uintptr(pending), 0, 0, 0, 0)
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 {
			return fmt.Errorf("io_uring_enter: %w", errno)
		}
		return nil
	}
}

// wait blocks until at least one completion is posted or timeout passes. A timeout or a
// signal is not an error; the caller re-checks the CQ and its own stop conditions.
func (r *ring) wait(timeout time.Duration) error {
	r.ts = syscall.NsecToTimespec(timeout.Nanoseconds())
	_, _, errno := syscall.Syscall6(sysIoUringEnter, uintptr(r.fd), 0, 1, ringEnterGetEvents|ringEnterExtArg,
		uintptr(unsafe.Pointer(&r.arg)), unsafe.Sizeof(r.arg))
	switch errno {
	case 0, syscall.ETIME, syscall.EINTR:
		return nil
	}
	return fmt.Errorf("io_uring_enter: %w", errno)
}

// next pops one completion; ok is false when the CQ is empty.
func (r *ring) next() (userData uint64, res int32, ok bool) {
	head := atomic.LoadUint32(r.cqHead)
	if head == atomic.LoadUint32(r.cqTail) {
		return 0, 0, false
	}
	cqe := unsafe.Add(r.cqes, uintptr(head&r.cqMask)*cqeSize)
	userData = *(*uint64)(cqe)
	res = *(*int32)(unsafe.Add(cqe, 8))
	atomic.StoreUint32(r.cqHead, head+1)
	return userData, res, true
}

func (r *ring) close() error {
	var errs []error
	for _, m := range r.maps {
		errs = append(errs, unix.Munmap(m))
	}
	r.maps = nil
	return errors.Join(append(errs, syscall.Close(r.fd))...)
}

// mmap maps one of the ring's regions and remembers it for close.
func (r *ring) mmap(offset int64, length int) (unsafe.Pointer, error) {
	m, err := unix.Mmap(r.fd, offset, length, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		r.close()
		return nil, fmt.Errorf("mmap io_uring region %#x: %w", offset, err)
	}
	r.maps = append(r.maps, m)
	return unsafe.Pointer(unsafe.SliceData(m)), nil
}
