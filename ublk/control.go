package ublk

import (
	"fmt"
	"os"
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

// control talks to /dev/ublk-control. Its command and data buffers are fields so their
// addresses are stable while the kernel reads or writes them.
type control struct {
	fd   int
	ring *ring
	cmd  ctrlCmd
	info devInfo
	par  params
}

func openControl() (*control, error) {
	fd, err := syscall.Open(controlPath, syscall.O_RDWR, 0)
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
	c.ring.close()
	return syscall.Close(c.fd)
}

// addDevice registers a device and returns the id the kernel assigned.
func (c *control) addDevice(queues, depth, maxIO int) (uint32, error) {
	c.info = devInfo{
		NrHwQueues:    uint16(queues),
		QueueDepth:    uint16(depth),
		MaxIOBufBytes: uint32(maxIO),
		DevID:         devIDAuto,
		UblksrvPID:    int32(os.Getpid()),
		Flags:         featURingCmdCompInTask,
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
// one command at a time, so the next CQE is this command's result.
func (c *control) run(nr uint32) error {
	if err := c.ring.prepare(ioctl(nr, uint32(unsafe.Sizeof(c.cmd))), 0, unsafe.Pointer(&c.cmd), unsafe.Sizeof(c.cmd)); err != nil {
		return err
	}
	if err := c.ring.flush(); err != nil {
		return fmt.Errorf("ublk control %#x: %w", nr, err)
	}
	deadline := time.Now().Add(controlTimeout)
	for {
		if _, res, ok := c.ring.next(); ok {
			if res < 0 {
				return fmt.Errorf("ublk control %#x: %w", nr, syscall.Errno(-res))
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ublk control %#x: timed out after %s", nr, controlTimeout)
		}
		if err := c.ring.wait(controlWait); err != nil {
			return err
		}
	}
}
