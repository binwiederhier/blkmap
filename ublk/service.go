// Package ublk serves a Backend as a Linux block device through the kernel's ublk driver.
// It is a trimmed, in-tree descendant of github.com/ehrlich-b/go-ublk (MIT, Benjamin
// Ehrlich): an ioctl-encoded control plane on /dev/ublk-control, one io_uring and one OS
// thread per queue, and per-tag buffers sized to the maximum request.
package ublk

import (
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	defaultBlockSize  = 512
	defaultMaxIOSize  = 1 << 20
	defaultQueueDepth = 64
	defaultMaxQueues  = 4
	blockSizeLarge    = 4096
	// maxRangeSectors is the largest discard or write-zeroes request advertised (1 GiB).
	maxRangeSectors = (1 << 30) / sectorSize
	// charDeviceWait is how long to wait for udev to create /dev/ublkcN after ADD_DEV.
	charDeviceWait = 5 * time.Second
	charDevicePoll = 50 * time.Millisecond
)

var (
	// ErrReowned means the device id now belongs to another server, so nothing was deleted.
	ErrReowned = errors.New("ublk device id reused by another server")
)

// Backend is the storage a device is served from. Reads must fill the whole buffer and
// writes must consume it; a short count is reported to the kernel as an I/O error.
type Backend interface {
	io.ReaderAt
	io.WriterAt
	Size() int64
	Flush() error
}

// Discarder is implemented by backends that can drop a range (TRIM); it is advertised to
// the kernel only when present.
type Discarder interface {
	Discard(off, length int64) error
}

// ZeroWriter is implemented by backends that can zero a range without a data transfer.
type ZeroWriter interface {
	WriteZeroes(off, length int64) error
}

// Params configures a device. Zero values take the defaults below.
type Params struct {
	Backend    Backend
	BlockSize  int // logical block size: 512 (default) or 4096
	MaxIOSize  int // largest request; default 1 MiB
	QueueDepth int // in-flight requests per queue; default 64
	NumQueues  int // default: CPUs, at most 4
	ReadOnly   bool
}

// Device is a live ublk block device.
type Device struct {
	ID        uint32
	BlockPath string // /dev/ublkbN
	CharPath  string // /dev/ublkcN
	params    *Params
	charFd    int
	queues    []*queue
	stopping  atomic.Bool
	stopped   bool
	deleted   bool
	failed    chan struct{} // closed when a queue loop died while the device was live
	failErr   error
	failOnce  sync.Once
}

// Info is the kernel's view of a device.
type Info struct {
	Live      bool // serving I/O; false once the server died or stopped it
	ServerPID int
}

// Create registers the device with the kernel, starts its queues, and returns once
// /dev/ublkbN is live.
func Create(p *Params) (*Device, error) {
	p.defaults()
	if err := p.validate(); err != nil {
		return nil, err
	}
	ctl, err := openControl()
	if err != nil {
		return nil, err
	}
	defer ctl.close()
	id, err := ctl.addDevice(p.NumQueues, p.QueueDepth, p.MaxIOSize)
	if err != nil {
		return nil, err
	}
	d := &Device{ID: id, BlockPath: fmt.Sprintf("%s%d", blockPrefix, id), CharPath: fmt.Sprintf("%s%d", charPrefix, id), params: p, charFd: -1, failed: make(chan struct{})}
	fail := func(err error) (*Device, error) {
		d.teardown(ctl)
		return nil, err
	}
	// The kernel may grant fewer queues than asked (it clamps to online CPUs)
	info, err := ctl.deviceInfo(id)
	if err != nil {
		return fail(err)
	}
	if err := ctl.setParams(id, buildParams(p)); err != nil {
		return fail(err)
	}
	if d.charFd, err = openCharDevice(d.CharPath); err != nil {
		return fail(err)
	}
	for i := 0; i < int(info.NrHwQueues); i++ {
		q, err := newQueue(d, uint16(i))
		if err != nil {
			return fail(err)
		}
		d.queues = append(d.queues, q)
		ready := make(chan error, 1)
		go q.run(ready)
		if err := <-ready; err != nil {
			return fail(fmt.Errorf("queue %d: %w", i, err))
		}
	}
	if err := ctl.startDevice(id); err != nil {
		return fail(err)
	}
	return d, nil
}

// Close is Stop followed by Delete. Deletion waits until nothing holds the block device
// open any more (a mount, a process with the device open), so unmount first.
func (d *Device) Close() error {
	ctl, err := openControl()
	if err != nil {
		return err
	}
	defer ctl.close()
	return d.teardown(ctl)
}

// Stop ends I/O: STOP_DEV drains in-flight requests and removes /dev/ublkbN, the queue
// threads exit and every char device fd is closed. The backend receives no calls after
// Stop returns, so it can be flushed and closed safely. Idempotent.
func (d *Device) Stop() error {
	ctl, err := openControl()
	if err != nil {
		// Without the control device the queues are still joined and the char device
		// released, which is what makes the kernel give up on the device
		return errors.Join(err, d.stop(nil))
	}
	defer ctl.close()
	return d.stop(ctl)
}

// Done is closed when a queue loop died while the device was live. Such a device answers
// no requests any more and should be closed; Err says why.
func (d *Device) Done() <-chan struct{} {
	return d.failed
}

// Err returns why the device failed, or nil.
func (d *Device) Err() error {
	select {
	case <-d.failed:
		return d.failErr
	default:
		return nil
	}
}

// Delete removes the kernel device (DEL_DEV). It blocks while anything still holds the
// block device open, which is why it is separate from Stop. Idempotent. If the id has
// meanwhile been handed to a successor (the kernel reuses the lowest free id, so a
// restarting server can own "our" number before our shutdown finishes), Delete leaves it
// alone and returns ErrReowned.
func (d *Device) Delete() error {
	ctl, err := openControl()
	if err != nil {
		return err
	}
	defer ctl.close()
	return d.delete(ctl)
}

// teardown is the shutdown sequence shared by Close and a failed Create.
func (d *Device) teardown(ctl *control) error {
	return errors.Join(d.stop(ctl), d.delete(ctl))
}

// stop issues STOP_DEV, which drains in-flight requests (the still-running loops serve
// them) and then aborts the outstanding fetches so the loops exit; then it releases every
// char device fd, which DEL_DEV later needs closed.
func (d *Device) stop(ctl *control) error {
	if d.stopped {
		return nil
	}
	d.stopped = true
	var errs []error
	if ctl != nil {
		if err := ctl.stopDevice(d.ID); err != nil && !errors.Is(err, syscall.ENODEV) {
			errs = append(errs, err)
		}
	}
	d.stopping.Store(true)
	for _, q := range d.queues {
		q.join()
		errs = append(errs, q.err, q.close())
	}
	d.queues = nil
	if d.charFd >= 0 {
		errs = append(errs, syscall.Close(d.charFd))
		d.charFd = -1
	}
	return errors.Join(errs...)
}

func (d *Device) delete(ctl *control) error {
	if d.deleted {
		return nil
	}
	if !d.stopped {
		if err := d.stop(ctl); err != nil {
			return err
		}
	}
	d.deleted = true
	// Our device is dead after stop; a live one under this id belongs to a successor
	if info, err := ctl.deviceInfo(d.ID); err == nil && (info.State == stateLive || (info.UblksrvPID > 0 && int(info.UblksrvPID) != os.Getpid())) {
		return ErrReowned
	}
	if err := ctl.deleteDevice(d.ID); err != nil && !errors.Is(err, syscall.ENODEV) {
		return err
	}
	return nil
}

// fail records the first queue failure and wakes Done.
func (d *Device) fail(err error) {
	d.failOnce.Do(func() {
		d.failErr = err
		close(d.failed)
	})
}

// defaults fills in the zero-value parameters.
func (p *Params) defaults() {
	if p.BlockSize == 0 {
		p.BlockSize = defaultBlockSize
	}
	if p.MaxIOSize == 0 {
		p.MaxIOSize = defaultMaxIOSize
	}
	if p.QueueDepth == 0 {
		p.QueueDepth = defaultQueueDepth
	}
	if p.NumQueues == 0 {
		p.NumQueues = min(runtime.NumCPU(), defaultMaxQueues)
	}
}

// validate rejects what the kernel would reject, with a readable reason.
func (p *Params) validate() error {
	if p.Backend == nil {
		return errors.New("ublk: a backend is required")
	}
	if p.BlockSize != defaultBlockSize && p.BlockSize != blockSizeLarge {
		return fmt.Errorf("ublk: block size must be %d or %d, got %d", defaultBlockSize, blockSizeLarge, p.BlockSize)
	}
	if size := p.Backend.Size(); size <= 0 {
		return fmt.Errorf("ublk: backend size must be positive, got %d", size)
	} else if size%int64(p.BlockSize) != 0 {
		return fmt.Errorf("ublk: backend size %d is not a multiple of the block size %d", size, p.BlockSize)
	}
	if page := os.Getpagesize(); p.MaxIOSize < page || p.MaxIOSize%p.BlockSize != 0 {
		return fmt.Errorf("ublk: max I/O size must be a multiple of the block size and at least a page (%d), got %d", page, p.MaxIOSize)
	}
	if p.QueueDepth < 1 || p.QueueDepth > maxQueueDepth {
		return fmt.Errorf("ublk: queue depth must be 1..%d, got %d", maxQueueDepth, p.QueueDepth)
	}
	if p.NumQueues < 1 {
		return fmt.Errorf("ublk: at least one queue is required")
	}
	return nil
}

// buildParams translates Params into the kernel's parameter block.
func buildParams(p *Params) *params {
	shift := uint8(bits.TrailingZeros(uint(p.BlockSize)))
	kp := &params{
		Types: paramTypeBasic,
		Basic: paramBasic{
			// A volatile cache is advertised because the backend's completed writes are not
			// necessarily durable; the kernel then issues flushes, which Backend.Flush honors
			Attrs:           attrVolatileCache,
			LogicalBSShift:  shift,
			PhysicalBSShift: shift,
			IOMinShift:      shift,
			MaxSectors:      uint32(p.MaxIOSize / sectorSize),
			DevSectors:      uint64(p.Backend.Size() / sectorSize),
		},
	}
	if p.ReadOnly {
		kp.Basic.Attrs |= attrReadOnly
	}
	_, canDiscard := p.Backend.(Discarder)
	_, canZero := p.Backend.(ZeroWriter)
	if canDiscard || canZero {
		kp.Types |= paramTypeDiscard
		kp.Discard = paramDiscard{DiscardGranularity: uint32(p.BlockSize), MaxDiscardSegments: 1}
		if canDiscard {
			kp.Discard.MaxDiscardSectors = maxRangeSectors
		}
		if canZero {
			kp.Discard.MaxWriteZeroesSectors = maxRangeSectors
		}
	}
	kp.Len = uint32(unsafe.Sizeof(*kp))
	return kp
}

// openCharDevice opens /dev/ublkcN, waiting briefly for udev to create the node.
func openCharDevice(path string) (int, error) {
	deadline := time.Now().Add(charDeviceWait)
	for {
		fd, err := syscall.Open(path, syscall.O_RDWR, 0)
		if err == nil {
			return fd, nil
		}
		if !errors.Is(err, syscall.ENOENT) || time.Now().After(deadline) {
			return -1, fmt.Errorf("open %s: %w", path, err)
		}
		time.Sleep(charDevicePoll)
	}
}

// GetInfo describes device id, or returns an error wrapping syscall.ENODEV if it does not exist.
func GetInfo(id uint32) (*Info, error) {
	ctl, err := openControl()
	if err != nil {
		return nil, err
	}
	defer ctl.close()
	info, err := ctl.deviceInfo(id)
	if err != nil {
		return nil, err
	}
	return &Info{Live: info.State == stateLive, ServerPID: int(info.UblksrvPID)}, nil
}

// Delete removes device id, stopping it first if it is still live. It is how a device left
// behind by a crashed server is cleaned up; it must not be used on a device whose server
// is still running, because DEL_DEV blocks while /dev/ublkcN is held open.
func Delete(id uint32) error {
	ctl, err := openControl()
	if err != nil {
		return err
	}
	defer ctl.close()
	if err := ctl.stopDevice(id); err != nil && !errors.Is(err, syscall.ENODEV) {
		return err
	}
	return ctl.deleteDevice(id)
}
