package ublk

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"sync"
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
	// userDataWake marks the eventfd read that wakes the queue thread for completions;
	// request completions carry their tag (< maxQueueDepth) instead.
	userDataWake = ^uint64(0)
	// Reads are served inline on the queue thread while the backend answers fast (a local
	// file: a few microseconds, where a worker handoff would cost more than the work), and
	// handed to workers once the smoothed read service time passes parallelAbove (a network
	// source: milliseconds, where only concurrency fills the pipe). The queue returns to
	// inline only after inlineAfter consecutive reads under fastRead: a mean would flip
	// back on a run of cache hits and then stall every request behind the next slow read.
	// Writes, flushes and discards always run inline: they go to the local COW file, where
	// parallel read-modify-writes only contend on the inode lock.
	parallelAbove = 250 * time.Microsecond
	fastRead      = 100 * time.Microsecond
	inlineAfter   = 1024
	// serviceSmoothing is the EWMA weight (1/n) of the service time estimate.
	serviceSmoothing = 8
)

var (
	errUnsupported = errors.New("unsupported request")
)

// queue serves one hardware queue: its own io_uring, descriptor mapping, per-tag buffers,
// OS thread and worker pool. The kernel requires every FETCH/COMMIT to come from the thread
// that first fetched on the queue. For a fast backend that thread serves requests itself;
// for a slow one it hands each request to a worker goroutine (one backend call per tag may
// be in flight, i.e. up to the queue depth concurrently) and commits results the workers
// post back, woken through an eventfd read on the same ring.
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

	efd       int     // eventfd the workers write to
	efdBuf    [8]byte // target of the standing eventfd read
	work      chan uint16
	workers   sync.WaitGroup
	results   []int32     // per tag, written by the worker before posting the tag
	durations []int64     // per tag, nanoseconds the backend call took
	completed []uint16    // tags whose backend call finished, waiting for COMMIT
	compMu    sync.Mutex  // Protects completed
	service   int64       // smoothed backend read service time in nanoseconds (queue thread only)
	fastRun   int         // consecutive reads under fastRead (queue thread only)
	parallel  bool        // whether reads currently go to the workers (queue thread only)
	shown     atomic.Bool // parallel, for Stats
}

func newQueue(d *Device, id uint16) (*queue, error) {
	fd, err := dupCloexec(d.charFd)
	if err != nil {
		return nil, err
	}
	depth := d.params.QueueDepth
	q := &queue{id: id, dev: d, fd: fd, efd: -1, cmds: make([]ioCmd, depth), done: make(chan struct{}),
		work: make(chan uint16, depth), results: make([]int32, depth), durations: make([]int64, depth), completed: make([]uint16, 0, depth)}
	if q.ring, err = newRing(uint32(depth+1), fd); err != nil { // +1 for the eventfd read
		q.close()
		return nil, err
	}
	if q.efd, err = unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK); err != nil {
		q.close()
		return nil, fmt.Errorf("eventfd: %w", err)
	}
	// The descriptor array sits at a fixed per-queue stride in the char device's mmap space
	descLen := pageRound(depth * int(unsafe.Sizeof(ioDesc{})))
	q.descs, err = unix.Mmap(fd, int64(id)*int64(descMmapStride), descLen, unix.PROT_READ, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		q.close()
		return nil, fmt.Errorf("mmap queue %d descriptors: %w", id, err)
	}
	q.bufs, err = unix.Mmap(-1, 0, depth*d.params.MaxIOSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
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
	defer func() {
		// A loop that dies while the device is live leaves its requests unanswered forever;
		// the owner is told so it can tear the device down instead of serving a zombie
		if q.err != nil && !q.dev.stopping.Load() {
			q.dev.fail(fmt.Errorf("queue %d: %w", q.id, q.err))
		}
	}()
	for tag := range q.cmds {
		if err := q.prepare(cmdFetchReq, uint16(tag), 0); err != nil {
			ready <- err
			return
		}
	}
	if err := q.armWake(); err != nil {
		ready <- err
		return
	}
	if err := q.ring.flush(); err != nil {
		ready <- err
		return
	}
	for i := 0; i < len(q.cmds); i++ {
		q.workers.Add(1)
		go q.worker()
	}
	defer q.workers.Wait()
	defer close(q.work)
	ready <- nil
	busy, aborted := 0, false
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
			switch {
			case userData == userDataWake:
				busy -= q.commitCompleted()
				if err := q.armWake(); err != nil {
					q.err = err
					return
				}
			case userData >= uint64(len(q.cmds)):
				log.Printf("ublk queue %d: completion for unknown tag %d ignored", q.id, userData)
			case res == resultAbort: // the kernel is aborting the queue (device stopping)
				aborted = true
			case res < 0:
				// A failed FETCH/COMMIT for one tag: that tag is dead, the others keep
				// serving. Exiting here instead would wedge every request on this queue.
				log.Printf("ublk queue %d tag %d: command failed: %s", q.id, userData, syscall.Errno(-res).Error())
			case q.parallel && q.op(uint16(userData)) == opRead:
				busy++
				q.work <- uint16(userData) // never blocks: at most depth tags are outstanding
			default:
				tag := uint16(userData)
				start := time.Now()
				res := q.serve(tag)
				if q.op(tag) == opRead {
					q.observe(time.Since(start))
				}
				if err := q.prepare(cmdCommitAndFetchReq, tag, res); err != nil {
					q.err = err
				}
			}
		}
		if err := q.ring.flush(); err != nil {
			q.err = err
			return
		}
		// Leave only once every request handed to a worker has been committed; the kernel
		// drains those before it aborts the idle fetches anyway
		if (aborted || q.dev.stopping.Load()) && busy == 0 {
			return
		}
	}
}

// worker serves requests until the work channel closes.
func (q *queue) worker() {
	defer q.workers.Done()
	for tag := range q.work {
		start := time.Now()
		q.results[tag] = q.serve(tag)
		q.durations[tag] = int64(time.Since(start))
		q.compMu.Lock()
		q.completed = append(q.completed, tag)
		q.compMu.Unlock()
		unix.Write(q.efd, []byte{1, 0, 0, 0, 0, 0, 0, 0})
	}
}

// commitCompleted prepares COMMIT_AND_FETCH for every finished request and returns how many.
func (q *queue) commitCompleted() int {
	q.compMu.Lock()
	defer q.compMu.Unlock()
	for _, tag := range q.completed {
		q.observe(time.Duration(q.durations[tag]))
		if err := q.prepare(cmdCommitAndFetchReq, tag, q.results[tag]); err != nil {
			q.err = err
		}
	}
	n := len(q.completed)
	q.completed = q.completed[:0]
	return n
}

// observe folds one backend service time into the estimate and picks the dispatch mode.
func (q *queue) observe(d time.Duration) {
	q.service += (int64(d) - q.service) / serviceSmoothing
	if d > fastRead {
		q.fastRun = 0
	} else {
		q.fastRun++
	}
	if !q.parallel && q.service > int64(parallelAbove) {
		q.parallel = true
		q.shown.Store(true)
	} else if q.parallel && q.fastRun >= inlineAfter {
		q.parallel, q.fastRun = false, 0
		q.shown.Store(false)
	}
}

// op returns the request operation of tag.
func (q *queue) op(tag uint16) uint32 {
	base := unsafe.Add(unsafe.Pointer(unsafe.SliceData(q.descs)), uintptr(tag)*unsafe.Sizeof(ioDesc{}))
	return atomic.LoadUint32((*uint32)(base)) & opMask
}

// armWake prepares the standing eventfd read whose completion wakes the loop.
func (q *queue) armWake() error {
	return q.ring.prepareRead(q.efd, unsafe.Pointer(&q.efdBuf[0]), uint32(len(q.efdBuf)), userDataWake)
}

// serve runs the request behind tag against the backend and returns the result to commit.
// A data request outside the device is refused before the backend sees it; the kernel never
// sends one, but the descriptor is shared memory and the backend is user code. A flush
// carries no range (the kernel sets its sector to -1), so it is not range checked.
func (q *queue) serve(tag uint16) int32 {
	d := q.desc(tag)
	size := q.dev.params.Backend.Size()
	if op := d.OpFlags & opMask; op != opFlush && (d.StartSector > uint64(size)/sectorSize || int64(d.NrSectors)*sectorSize > size-int64(d.StartSector)*sectorSize) {
		return resultEIO
	}
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
	if errors.Is(err, errUnsupported) {
		return resultEOpNotSupp
	}
	if err != nil {
		return resultEIO
	}
	return int32(length)
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
// Addr is left out: the buffer address is ours, not the kernel's.
func (q *queue) desc(tag uint16) ioDesc {
	base := unsafe.Add(unsafe.Pointer(unsafe.SliceData(q.descs)), uintptr(tag)*unsafe.Sizeof(ioDesc{}))
	return ioDesc{
		OpFlags:     atomic.LoadUint32((*uint32)(base)),
		NrSectors:   atomic.LoadUint32((*uint32)(unsafe.Add(base, unsafe.Offsetof(ioDesc{}.NrSectors)))),
		StartSector: atomic.LoadUint64((*uint64)(unsafe.Add(base, unsafe.Offsetof(ioDesc{}.StartSector)))),
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
	if q.efd >= 0 {
		errs = append(errs, syscall.Close(q.efd))
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
