// Package device ties a config to a live ublk block device: it opens the sources and the COW
// store, creates /dev/ublkbN through go-ublk, and publishes it as /dev/blkmap/<id>.
package device

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/ehrlich-b/go-ublk"

	"heckel.io/blkmap/config"
	"heckel.io/blkmap/cow"
	"heckel.io/blkmap/source"
)

const (
	// DevDir is where device symlinks are published: /dev/blkmap/<id> -> /dev/ublkbN.
	DevDir = "/dev/blkmap"
	// numQueues and queueDepth bound the kernel-side parallelism. maxIOSize MUST stay at
	// go-ublk's per-tag buffer size (64 KiB): for larger requests the library reads into a
	// pooled buffer but commits the per-tag address, so the kernel sees stale data (verified
	// 2026-10-01 with 1 MiB reads returning zeros). The library's own examples do the same.
	numQueues  = 2
	queueDepth = 64
	maxIOSize  = ublk.IOBufferSizePerTag
	devDirMode = 0755
)

// Device is a running blkmap block device.
type Device struct {
	Path      string // the published symlink, e.g. /dev/blkmap/<id>
	BlockPath string // the kernel device, e.g. /dev/ublkb0
	store     *cow.Store
	ublk      *ublk.Device
}

// Start opens the sources and COW store for c and serves them as a block device, publishing
// the symlink in devDir. It returns once the kernel device is live.
func Start(ctx context.Context, c *config.Config, devDir string) (*Device, error) {
	base, err := source.FromConfig(c)
	if err != nil {
		return nil, err
	}
	store, err := cow.Open(base, c.COW.File, c.COW.Bitmap, c.COW.ChunkSize)
	if err != nil {
		base.Close()
		return nil, err
	}
	params := ublk.DefaultParams(&backend{Backend: store, id: c.ID})
	params.NumQueues = numQueues
	params.QueueDepth = queueDepth
	params.MaxIOSize = maxIOSize
	params.LogicalBlockSize = c.BlockSize
	params.ReadOnly = c.ReadOnly
	dev, err := ublk.CreateAndServe(ctx, params, &ublk.Options{Logger: logger{}})
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("ublk: %w", err)
	}
	d := &Device{Path: filepath.Join(devDir, c.ID), BlockPath: dev.BlockPath(), store: store, ublk: dev}
	if err := d.publish(devDir); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// Written returns the number of COW chunks written so far.
func (d *Device) Written() int64 {
	return d.store.Written()
}

// Close stops the device, removes the symlink, and closes the store and sources.
func (d *Device) Close() error {
	var errs []error
	if err := os.Remove(d.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	// Stop the kernel device before the store: STOP_DEV drains in-flight I/O, which still
	// needs the store open
	errs = append(errs, d.ublk.Close(), d.store.Close())
	return errors.Join(errs...)
}

// publish creates the /dev/blkmap/<id> symlink, replacing a stale one from a previous run.
func (d *Device) publish(devDir string) error {
	if err := os.MkdirAll(devDir, devDirMode); err != nil {
		return err
	}
	if err := os.Remove(d.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Symlink(d.BlockPath, d.Path)
}

// logger routes go-ublk's diagnostics to the standard logger; debug lines are dropped.
type logger struct{}

func (logger) Printf(format string, args ...any) {
	log.Printf("ublk: "+format, args...)
}

func (logger) Debugf(string, ...any) {
}
