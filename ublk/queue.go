package ublk

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	// queueWait bounds one parked io_uring_enter so the loop notices a stop request even
	// when no I/O and no abort ever wake it.
	queueWait = 100 * time.Millisecond
)

var (
	errUnsupported = errors.New("unsupported request")
)

// queue serves one hardware queue: its own io_uring, descriptor mapping, per-tag buffers
// and OS thread.
type queue struct {
	id    uint16
	dev   *Device
	fd    int // dup of the char device fd, so each ring has its own target
	ring  *ring
	descs []byte // the kernel's request descriptors for this queue, read-only
	bufs  []byte // depth x maxIO of request data, shared with the kernel by address
	cmds  []ioCmd
	done  chan struct{}
	err   error // why the loop exited, if not because of a stop
}

func newQueue(d *Device, id uint16) (*queue, error) {
	fd, err := syscall.Dup(d.charFd)
	if err != nil {
		return nil, err
	}
	q := &queue{id: id, dev: d, fd: fd, cmds: make([]ioCmd, d.params.QueueDepth), done: make(chan struct{})}
	if q.ring, err = newRing(uint32(d.params.QueueDepth), fd); err != nil {
		q.close()
		return nil, err
	}
	// The descriptor array sits at a fixed per-queue stride in the char device's mmap space
	descLen := pageRound(d.params.QueueDepth * int(unsafe.Sizeof(ioDesc{})))
	q.descs, err = unix.Mmap(fd, int64(id)*int64(descMmapStride), descLen, unix.PROT_READ, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		q.close()
		return nil, fmt.Errorf("mmap queue %d descriptors: %w", id, err)
	}
	q.bufs, err = unix.Mmap(-1, 0, d.params.QueueDepth*d.params.MaxIOSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		q.close()
		return nil, fmt.Errorf("allocate queue %d buffers: %w", id, err)
	}
	return q, nil
}

// run is the queue's daemon loop. ublk binds a queue to the thread that fetches on it
// first, so the goroutine stays locked to its OS thread for the device's whole life. ready
// receives the outcome of priming the queue.
func (q *queue) run(ready chan<- error) {
	runtime.LockOSThread()
	defer close(q.done)
	for tag := range q.cmds {
		if err := q.prepare(cmdFetchReq, uint16(tag), 0); err != nil {
			ready <- err
			return
		}
	}
	if err := q.ring.flush(); err != nil {
		ready <- err
		return
	}
	ready <- nil
	for {
		if err := q.ring.wait(queueWait); err != nil {
			q.err = err
			return
		}
		for {
			userData, res, ok := q.ring.next()
			if !ok {
				break
			}
			if res == resultAbort { // the kernel is aborting the queue (device stopping)
				return
			}
			if res < 0 {
				// A failed FETCH/COMMIT for one tag: that tag is dead, the others keep
				// serving. Exiting here instead would wedge every request on this queue.
				log.Printf("ublk queue %d tag %d: command failed: %s", q.id, userData, syscall.Errno(-res).Error())
				q.err = fmt.Errorf("queue %d tag %d: %w", q.id, userData, syscall.Errno(-res))
				continue
			}
			q.handle(uint16(userData))
		}
		if err := q.ring.flush(); err != nil {
			q.err = err
			return
		}
		if q.dev.stopping.Load() {
			return
		}
	}
}

// handle serves the request behind tag and prepares its completion.
func (q *queue) handle(tag uint16) {
	d := q.desc(tag)
	off := int64(d.StartSector) * sectorSize
	length := int64(d.NrSectors) * sectorSize
	buf := q.buf(tag)
	var err error
	switch d.OpFlags & opMask {
	case opRead:
		if length > int64(len(buf)) {
			err = fmt.Errorf("read of %d bytes exceeds the %d-byte request buffer", length, len(buf))
		} else {
			n, rerr := q.dev.params.Backend.ReadAt(buf[:length], off)
			err = transferResult(n, int(length), rerr)
		}
	case opWrite:
		if length > int64(len(buf)) {
			err = fmt.Errorf("write of %d bytes exceeds the %d-byte request buffer", length, len(buf))
		} else {
			n, werr := q.dev.params.Backend.WriteAt(buf[:length], off)
			err = transferResult(n, int(length), werr)
		}
	case opFlush:
		err = q.dev.params.Backend.Flush()
	case opDiscard:
		if b, ok := q.dev.params.Backend.(Discarder); ok {
			err = b.Discard(off, length)
		} else {
			err = errUnsupported
		}
	case opWriteZeroes:
		if b, ok := q.dev.params.Backend.(ZeroWriter); ok {
			err = b.WriteZeroes(off, length)
		} else {
			err = errUnsupported
		}
	default:
		err = errUnsupported
	}
	res := int32(length)
	if errors.Is(err, errUnsupported) {
		res = resultEOpNotSupp
	} else if err != nil {
		res = resultEIO
	}
	if err := q.prepare(cmdCommitAndFetchReq, tag, res); err != nil {
		q.err = err
	}
}

// prepare queues a FETCH or COMMIT_AND_FETCH for tag; the SQE goes out with the next flush.
func (q *queue) prepare(nr uint32, tag uint16, res int32) error {
	c := &q.cmds[tag]
	c.QID = q.id
	c.Tag = tag
	c.Result = res
	c.Addr = uint64(uintptr(unsafe.Pointer(unsafe.SliceData(q.buf(tag)))))
	return q.ring.prepare(ioctl(nr, uint32(unsafe.Sizeof(*c))), uint64(tag), unsafe.Pointer(c), unsafe.Sizeof(*c))
}

// desc reads tag's descriptor with atomic loads, since the kernel wrote it from another CPU.
func (q *queue) desc(tag uint16) ioDesc {
	base := unsafe.Add(unsafe.Pointer(unsafe.SliceData(q.descs)), uintptr(tag)*unsafe.Sizeof(ioDesc{}))
	return ioDesc{
		OpFlags:     atomic.LoadUint32((*uint32)(base)),
		NrSectors:   atomic.LoadUint32((*uint32)(unsafe.Add(base, unsafe.Offsetof(ioDesc{}.NrSectors)))),
		StartSector: atomic.LoadUint64((*uint64)(unsafe.Add(base, unsafe.Offsetof(ioDesc{}.StartSector)))),
		Addr:        atomic.LoadUint64((*uint64)(unsafe.Add(base, unsafe.Offsetof(ioDesc{}.Addr)))),
	}
}

func (q *queue) buf(tag uint16) []byte {
	size := q.dev.params.MaxIOSize
	return q.bufs[int(tag)*size : (int(tag)+1)*size]
}

// join waits for the loop to exit. Callers issue STOP_DEV or set stopping first.
func (q *queue) join() {
	<-q.done
}

func (q *queue) close() error {
	var errs []error
	if q.ring != nil {
		errs = append(errs, q.ring.close())
	}
	if q.descs != nil {
		errs = append(errs, unix.Munmap(q.descs))
	}
	if q.bufs != nil {
		errs = append(errs, unix.Munmap(q.bufs))
	}
	return errors.Join(append(errs, syscall.Close(q.fd))...)
}

// transferResult turns a backend ReadAt/WriteAt outcome into a request result.
func transferResult(n, length int, err error) error {
	if n == length && (err == nil || errors.Is(err, io.EOF)) {
		return nil
	}
	if err != nil {
		return err
	}
	return io.ErrUnexpectedEOF
}

func pageRound(n int) int {
	page := os.Getpagesize()
	return (n + page - 1) / page * page
}
