package ublk

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	// controlEntries is the ring size for the control device; commands are synchronous.
	controlEntries = 4
	// controlTimeout bounds one control command; STOP_DEV on a busy device can take a while.
	controlTimeout = 30 * time.Second
	controlWait    = 100 * time.Millisecond
)

var (
	errControlPoisoned = errors.New("ublk control: a previous command never completed")
	// leaked pins controls whose command timed out: the kernel may still write into their
	// buffers, so they are never freed or closed.
	leaked   []*control
	leakedMu sync.Mutex
)

// control talks to /dev/ublk-control. Its command and data buffers are fields so their
// addresses are stable while the kernel reads or writes them.
type control struct {
	fd       int
	ring     *ring
	seq      uint64 // user data of the command in flight, so a late completion is told apart
	poisoned bool
	cmd      ctrlCmd
	info     devInfo
	par      params
	feat     uint64
}

func openControl() (*control, error) {
	fd, err := openCloexec(controlPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w (is ublk_drv loaded?)", controlPath, err)
	}
	r, err := newRing(controlEntries, fd)
	if err != nil {
		syscall.Close(fd)
		return nil, err
	}
	return &control{fd: fd, ring: r}, nil
}

func (c *control) close() error {
	if c.poisoned {
		leakedMu.Lock()
		leaked = append(leaked, c)
		leakedMu.Unlock()
		return nil
	}
	c.ring.close()
	return syscall.Close(c.fd)
}

// addDevice registers a device and returns the id the kernel assigned.
func (c *control) addDevice(queues, depth, maxIO int, flags uint64) (uint32, error) {
	c.info = devInfo{
		NrHwQueues:    uint16(queues),
		QueueDepth:    uint16(depth),
		MaxIOBufBytes: uint32(maxIO),
		DevID:         devIDAuto,
		UblksrvPID:    int32(os.Getpid()),
		Flags:         flags,
		OwnerUID:      uint32(os.Getuid()),
		OwnerGID:      uint32(os.Getgid()),
	}
	c.cmd = ctrlCmd{DevID: devIDAuto, QueueID: queueIDControl, Len: uint16(unsafe.Sizeof(c.info)), Addr: uint64(uintptr(unsafe.Pointer(&c.info)))}
	if err := c.run(cmdAddDev); err != nil {
		return 0, err
	}
	return c.info.DevID, nil
}

// deviceInfo fetches the kernel's view of the device, notably the granted queue count.
func (c *control) deviceInfo(id uint32) (*devInfo, error) {
	c.info = devInfo{}
	c.cmd = ctrlCmd{DevID: id, QueueID: queueIDControl, Len: uint16(unsafe.Sizeof(c.info)), Addr: uint64(uintptr(unsafe.Pointer(&c.info)))}
	if err := c.run(cmdGetDevInfo); err != nil {
		return nil, err
	}
	info := c.info
	return &info, nil
}

func (c *control) setParams(id uint32, p *params) error {
	c.par = *p
	c.cmd = ctrlCmd{DevID: id, QueueID: queueIDControl, Len: uint16(unsafe.Sizeof(c.par)), Addr: uint64(uintptr(unsafe.Pointer(&c.par)))}
	return c.run(cmdSetParams)
}

// getParams reads the kernel's parameter block of the device.
func (c *control) getParams(id uint32) (*params, error) {
	c.par = params{Len: uint32(unsafe.Sizeof(c.par))}
	c.cmd = ctrlCmd{DevID: id, QueueID: queueIDControl, Len: uint16(unsafe.Sizeof(c.par)), Addr: uint64(uintptr(unsafe.Pointer(&c.par)))}
	if err := c.run(cmdGetParams); err != nil {
		return nil, err
	}
	p := c.par
	return &p, nil
}

// features returns the kernel's supported feature flags.
func (c *control) features() (uint64, error) {
	c.feat = 0
	c.cmd = ctrlCmd{DevID: devIDAuto, QueueID: queueIDControl, Len: uint16(unsafe.Sizeof(c.feat)), Addr: uint64(uintptr(unsafe.Pointer(&c.feat)))}
	if err := c.run(cmdGetFeatures); err != nil {
		return 0, err
	}
	return c.feat, nil
}

func (c *control) startUserRecovery(id uint32) error {
	c.cmd = ctrlCmd{DevID: id, QueueID: queueIDControl}
	return c.run(cmdStartUserRecovery)
}

// endUserRecovery completes a recovery once every queue has fetched; the kernel records
// this process as the server and lets the waiting I/O through.
func (c *control) endUserRecovery(id uint32) error {
	c.cmd = ctrlCmd{DevID: id, QueueID: queueIDControl, Data: uint64(os.Getpid())}
	return c.run(cmdEndUserRecovery)
}

func (c *control) startDevice(id uint32) error {
	c.cmd = ctrlCmd{DevID: id, QueueID: queueIDControl, Data: uint64(os.Getpid())}
	return c.run(cmdStartDev)
}

func (c *control) stopDevice(id uint32) error {
	c.cmd = ctrlCmd{DevID: id, QueueID: queueIDControl}
	return c.run(cmdStopDev)
}

func (c *control) deleteDevice(id uint32) error {
	c.cmd = ctrlCmd{DevID: id, QueueID: queueIDControl}
	return c.run(cmdDelDev)
}

// run submits c.cmd as command nr and waits for its completion. The control ring carries
// one command at a time; a completion with another sequence number is a stale one from a
// command that timed out and is skipped. After a timeout the control is poisoned, since
// its buffers may still be written by the kernel.
func (c *control) run(nr uint32) error {
	if c.poisoned {
		return errControlPoisoned
	}
	c.seq++
	if err := c.ring.prepare(ctrlIoctl(nr), c.seq, unsafe.Pointer(&c.cmd), unsafe.Sizeof(c.cmd)); err != nil {
		return err
	}
	if err := c.ring.flush(); err != nil {
		return fmt.Errorf("ublk control %#x: %w", nr, err)
	}
	deadline := time.Now().Add(controlTimeout)
	for {
		if userData, res, ok := c.ring.next(); ok {
			if userData != c.seq {
				continue
			}
			if res < 0 {
				return fmt.Errorf("ublk control %#x: %w", nr, syscall.Errno(-res))
			}
			return nil
		}
		if time.Now().After(deadline) {
			c.poisoned = true
			return fmt.Errorf("ublk control %#x: timed out after %s", nr, controlTimeout)
		}
		if err := c.ring.wait(controlWait); err != nil {
			return err
		}
	}
}
