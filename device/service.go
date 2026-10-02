// Package device ties a config to a live ublk block device: it opens the sources and the COW
// store, creates /dev/ublkbN through the ublk package, and publishes it as /dev/blkmap/<id>.
package device

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"heckel.io/blkmap/config"
	"heckel.io/blkmap/cow"
	"heckel.io/blkmap/source"
	"heckel.io/blkmap/ublk"
	"heckel.io/blkmap/util"
)

const (
	// DevDir is where device symlinks are published: /dev/blkmap/<id> -> /dev/ublkbN.
	DevDir = "/dev/blkmap"
	// RunDir holds one file per device with its ublk id, so a restart after a crash can
	// delete the dead kernel device the previous server left behind.
	RunDir        = "/run/blkmap"
	devDirMode    = 0755
	stateFileMode = 0600
	// flushInterval bounds how long a completed write can sit without reaching disk when
	// the guest never issues a flush (raw dd, no filesystem).
	flushInterval = 5 * time.Second
	// readAheadKB is the kernel read-ahead window set on the block device. The default
	// (128 KiB) is tuned for disks; for a source that is a network round trip per request,
	// a window of several requests lets a sequential reader keep many in flight.
	readAheadKB = 4096
)

// Options describes a device to serve from an arbitrary read-only base. This is the library
// entry point: a program that computes or fetches blocks implements source.Source and
// hands it to Serve; the config-file path (Start) is built on it.
type Options struct {
	ID        string        // device name; the symlink is <DevDir>/<ID>
	Base      source.Source // read-only base image; Serve takes ownership and closes it
	COWFile   string        // overlay file; created if missing
	Bitmap    string        // defaults to COWFile + ".bitmap"
	ChunkSize int64         // COW granularity; defaults to config.DefaultChunkSize
	BlockSize int           // 512 (default) or 4096
	ReadOnly  bool
	DevDir    string   // defaults to DevDir
	RunDir    string   // where the ublk id is recorded; defaults to RunDir
	Hydrate   *Hydrate // background copy of the base into the COW file; nil = off
}

// Device is a running blkmap block device.
type Device struct {
	Path      string // the published symlink, e.g. /dev/blkmap/<id>
	BlockPath string // the kernel device, e.g. /dev/ublkb0
	statePath string // RunDir/<id>, holding the ublk id
	store     *cow.Store
	ublk      *ublk.Device
	stop      context.CancelFunc // ends the background goroutines (hydration, periodic flush)
	bg        sync.WaitGroup
	closed    bool
}

// Start opens the sources and COW store for c and serves them as a block device, publishing
// the symlink in devDir. It returns once the kernel device is live. A device whose bitmap
// says every chunk is already in the COW file is served without opening its sources.
func Start(ctx context.Context, c *config.Config, devDir string) (*Device, error) {
	hydrate, err := hydrateFromConfig(c.Hydrate)
	if err != nil {
		return nil, err
	}
	return startWithHydrate(ctx, c, devDir, RunDir, hydrate)
}

// startWithHydrate is Start with an explicit run directory and hydration plan.
func startWithHydrate(ctx context.Context, c *config.Config, devDir, runDir string, hydrate *Hydrate) (*Device, error) {
	var base source.Source
	size, chunkSize, complete, err := cow.Complete(c.COW.Bitmap)
	if err != nil {
		return nil, err
	}
	if complete && chunkSize == c.COW.ChunkSize && (c.Size == 0 || c.Size == size) {
		log.Printf("%s: fully hydrated (%s in %s), not opening the sources", c.ID, util.FormatSize(size), c.COW.File)
		base = source.NewZero(size)
		hydrate = nil
	} else if base, err = source.FromConfig(c); err != nil {
		return nil, err
	}
	return Serve(ctx, &Options{
		ID:        c.ID,
		Base:      base,
		COWFile:   c.COW.File,
		Bitmap:    c.COW.Bitmap,
		ChunkSize: c.COW.ChunkSize,
		BlockSize: c.BlockSize,
		ReadOnly:  c.ReadOnly,
		DevDir:    devDir,
		RunDir:    runDir,
		Hydrate:   hydrate,
	})
}

// Serve layers a COW store over o.Base and serves it as a block device. Serve owns o.Base
// from here on, even when it fails. The context only guards setup; the device lives until
// Close.
func Serve(ctx context.Context, o *Options) (*Device, error) {
	if o.Base == nil {
		return nil, errors.New("a base source is required")
	}
	if err := ctx.Err(); err != nil {
		o.Base.Close()
		return nil, err
	}
	if !config.ValidID(o.ID) {
		o.Base.Close()
		return nil, fmt.Errorf("invalid device id %q", o.ID)
	}
	if o.COWFile == "" {
		o.Base.Close()
		return nil, errors.New("a cow file is required")
	}
	bitmap, chunkSize, blockSize, devDir, runDir := o.Bitmap, o.ChunkSize, o.BlockSize, o.DevDir, o.RunDir
	if bitmap == "" {
		bitmap = o.COWFile + config.BitmapExt
	}
	if runDir == "" {
		runDir = RunDir
	}
	if chunkSize == 0 {
		chunkSize = config.DefaultChunkSize
	}
	if blockSize == 0 {
		blockSize = config.DefaultBlockSize
	}
	if devDir == "" {
		devDir = DevDir
	}
	statePath := filepath.Join(runDir, o.ID)
	deleteDeadPredecessor(o.ID, statePath)
	store, err := cow.Open(o.Base, o.COWFile, bitmap, chunkSize)
	if err != nil {
		o.Base.Close()
		return nil, err
	}
	b := &backend{store: store, id: o.ID}
	dev, err := ublk.Create(&ublk.Params{Backend: b, BlockSize: blockSize, ReadOnly: o.ReadOnly})
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("ublk: %w", err)
	}
	d := &Device{Path: filepath.Join(devDir, o.ID), BlockPath: dev.BlockPath, statePath: statePath, store: store, ublk: dev}
	if err := os.WriteFile(filepath.Join("/sys/block", filepath.Base(dev.BlockPath), "queue", "read_ahead_kb"), []byte(strconv.Itoa(readAheadKB)), 0); err != nil {
		log.Printf("%s: cannot set read-ahead: %s", o.ID, err.Error())
	}
	if err := d.publish(devDir); err != nil {
		d.Close()
		return nil, err
	}
	bgCtx, cancel := context.WithCancel(context.Background())
	d.stop = cancel
	d.bg.Add(1)
	go func() {
		defer d.bg.Done()
		d.flushLoop(bgCtx)
	}()
	if o.Hydrate != nil {
		h := newHydrator(o.ID, store, o.Base, o.Hydrate, b.busy)
		d.bg.Add(1)
		go func() {
			defer d.bg.Done()
			h.run(bgCtx)
		}()
	}
	return d, nil
}

// flushLoop makes completed writes durable every flushInterval, for guests that never
// flush themselves; the bitmap is only ever written after the COW data (see cow.Store.Flush).
func (d *Device) flushLoop(ctx context.Context) {
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if d.store.Dirty() {
				if err := d.store.Flush(); err != nil {
					log.Printf("%s: periodic flush failed: %s", filepath.Base(d.Path), err.Error())
				}
			}
		}
	}
}

// hydrateFromConfig turns the config block into a plan, reading the prefetch list.
func hydrateFromConfig(h *config.Hydrate) (*Hydrate, error) {
	if h == nil {
		return nil, nil
	}
	plan := &Hydrate{Rest: h.Rest, Rate: h.Rate, UseCache: h.UseCache, Concurrency: h.Concurrency, Report: h.ReportEvery}
	if h.PrefetchList != "" {
		var err error
		if plan.Prefetch, err = source.ParsePrefetchFile(h.PrefetchList); err != nil {
			return nil, err
		}
	}
	return plan, nil
}

// deleteDeadPredecessor removes the kernel device a previous server of this id left behind
// (recorded in statePath) if it is no longer being served; a crash leaves such devices
// around, since only DEL_DEV removes them.
func deleteDeadPredecessor(id, statePath string) {
	content, err := os.ReadFile(statePath)
	if err != nil {
		return
	}
	old, err := strconv.ParseUint(strings.TrimSpace(string(content)), 10, 32)
	if err != nil {
		return
	}
	info, err := ublk.GetInfo(uint32(old))
	if err != nil || info.Live && processAlive(info.ServerPID) {
		return
	}
	if err := ublk.Delete(uint32(old)); err != nil && !errors.Is(err, syscall.ENODEV) {
		log.Printf("%s: cannot delete stale ublk device %d: %s", id, old, err.Error())
		return
	}
	log.Printf("%s: deleted stale ublk device %d left by a previous server", id, old)
}

func processAlive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

// Size returns the device size in bytes.
func (d *Device) Size() int64 {
	return d.store.Size()
}

// Chunks returns the number of COW chunks in the device.
func (d *Device) Chunks() int64 {
	return d.store.Chunks()
}

// Written returns the number of COW chunks written so far.
func (d *Device) Written() int64 {
	return d.store.Written()
}

// Close shuts the device down in an order that cannot lose data: background work stops,
// STOP_DEV drains in-flight I/O (the store must still be open for that), the store flushes
// the COW file and then the bitmap and closes, and only then DEL_DEV, which can wait for
// openers of the block device. A kill during that wait finds everything already on disk.
func (d *Device) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	if d.stop != nil {
		d.stop()
		d.bg.Wait()
	}
	if at := mountPoint(d.BlockPath); at != "" {
		log.Printf("%s: still mounted at %s; unmount it, deletion waits for it", filepath.Base(d.Path), at)
	}
	errs := []error{d.ublk.Stop(), d.store.Close()}
	if err := d.ublk.Delete(); errors.Is(err, ublk.ErrReowned) {
		// A restarted server already owns this id, its symlink and its state file
		log.Printf("%s: kernel device id reused by a successor, leaving its files in place", filepath.Base(d.Path))
		return errors.Join(errs...)
	} else if err != nil {
		errs = append(errs, err)
	}
	// Only unpublish what is still ours: a successor started under the same name may
	// already have replaced the symlink and the state file
	if target, err := os.Readlink(d.Path); err == nil && target == d.BlockPath {
		if err := os.Remove(d.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if content, err := os.ReadFile(d.statePath); err == nil && strings.TrimSpace(string(content)) == strconv.FormatUint(uint64(d.ublk.ID), 10) {
		if err := os.Remove(d.statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// mountPoint returns where the block device is mounted, or "" if it is not.
func mountPoint(blockPath string) string {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		// ... "-" fstype source options: the source follows the separator
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == "-" && i+2 < len(fields) && fields[i+2] == blockPath && len(fields) > 4 {
				return fields[4]
			}
		}
	}
	return ""
}

// publish creates the /dev/blkmap/<id> symlink, replacing a stale one from a previous run,
// and records the ublk id for deleteDeadPredecessor.
func (d *Device) publish(devDir string) error {
	if err := os.MkdirAll(devDir, devDirMode); err != nil {
		return err
	}
	if err := os.Remove(d.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Symlink(d.BlockPath, d.Path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(d.statePath), devDirMode); err != nil {
		return err
	}
	return os.WriteFile(d.statePath, []byte(fmt.Sprintf("%d\n", d.ublk.ID)), stateFileMode)
}
