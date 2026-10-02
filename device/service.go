// Package device ties a config to a live ublk block device: it opens the sources and the COW
// store, creates /dev/ublkbN through the ublk package, and publishes it as /dev/blkmap/<id>.
package device

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// RunDir holds per-device runtime state (ublk id and server pid, the live bitmap, the
	// status socket), so a restarted server can re-attach to the device it left behind.
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
	// ExitDetached is the exit status after Detach; the unit restarts on it.
	ExitDetached = 75
	// liveBitmapExt names the live bitmap in RunDir (<id>.bitmap, see cow.Bitmap).
	liveBitmapExt   = ".bitmap"
	ublkBlockPrefix = "/dev/ublkb"
)

var (
	// served holds the IDs this process serves, so a second Serve of one is refused rather
	// than mistaken for a predecessor to recover
	served   = map[string]bool{}
	servedMu sync.Mutex
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
	// Identity fingerprints the base's content; once the COW file holds writes, Serve
	// refuses a base with another one (see cow.Options). Start fills it in from the sources;
	// empty skips the check.
	Identity string
	// Recovery lets the kernel device outlive this process: if it dies (or Detach hands it
	// off), I/O waits and the next Serve of the same ID re-attaches. Only for processes a
	// supervisor restarts; without one, I/O to a crashed device would hang.
	Recovery bool
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
	closed    atomic.Bool
	id        string
	base      source.Source
	backend   *backend
	hydrator  *hydrator // nil without hydration
	started   time.Time
	recovered bool      // re-attached to a running device
	status    io.Closer // the status socket; nil if it could not be opened
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
	var identity string // stays empty when detached: there are no sources to check
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
	} else {
		identity = source.Identity(base)
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
		Identity:  identity,
		Recovery:  true, // the systemd unit restarts the server
	})
}

// Serve layers a COW store over o.Base and serves it as a block device. Serve owns o.Base
// from here on, even when it fails. The context only guards setup; the device lives until
// Close.
func Serve(ctx context.Context, o *Options) (*Device, error) {
	// Before anything is opened: a cancelled start must not touch a predecessor
	if err := ctx.Err(); err != nil {
		if o.Base != nil {
			o.Base.Close()
		}
		return nil, err
	}
	store, pred, err := openStore(o)
	if err != nil {
		return nil, err
	}
	d, err := serveStore(ctx, o, store, pred, store)
	if err != nil {
		closeStore(o.ID, store, pred)
		return nil, err
	}
	return d, nil
}

// openStore validates the options, fills their defaults, claims the ID in this process, takes
// over a predecessor device and opens the COW store over the base; it owns o.Base from here
// on. What it returns is released with closeStore until a Device owns it.
func openStore(o *Options) (*cow.Store, *predecessor, error) {
	if o.Base == nil {
		return nil, nil, errors.New("a base source is required")
	}
	if !config.ValidID(o.ID) {
		o.Base.Close()
		return nil, nil, fmt.Errorf("invalid device id %q", o.ID)
	}
	if o.COWFile == "" {
		o.Base.Close()
		return nil, nil, errors.New("a cow file is required")
	}
	o.defaults()
	if !markServed(o.ID) {
		o.Base.Close()
		return nil, nil, fmt.Errorf("%s is already served by this process", o.ID)
	}
	if o.Recovery && !recoverySupported() {
		log.Printf("%s: the kernel lacks ublk user recovery; a crash fails I/O instead of pausing it", o.ID)
		o.Recovery = false
	}
	statePath := filepath.Join(o.RunDir, o.ID)
	pred, err := takeOver(o, statePath)
	if err != nil {
		o.Base.Close()
		unmarkServed(o.ID)
		return nil, nil, err
	}
	livePath := ""
	if o.Recovery {
		if err := os.MkdirAll(o.RunDir, devDirMode); err != nil {
			o.Base.Close()
			pred.drop(o.ID)
			unmarkServed(o.ID)
			return nil, nil, err
		}
		livePath = filepath.Join(o.RunDir, o.ID+liveBitmapExt)
	}
	store, err := cow.OpenWith(o.Base, &cow.Options{COWFile: o.COWFile, Bitmap: o.Bitmap, LiveBitmap: livePath, ChunkSize: o.ChunkSize, Identity: o.Identity})
	if err != nil {
		o.Base.Close()
		pred.drop(o.ID)
		unmarkServed(o.ID)
		return nil, nil, err
	}
	return store, pred, nil
}

// closeStore releases what openStore returned when no Device came to own it.
func closeStore(id string, store *cow.Store, pred *predecessor) {
	store.Close()
	pred.drop(id)
	unmarkServed(id)
}

// serveStore brings up the kernel device for an opened store, re-attaching to the predecessor
// when there is one; io is what the kernel talks to (the store itself, or a group router in
// front of it). The caller releases the store on failure.
func serveStore(ctx context.Context, o *Options, store *cow.Store, pred *predecessor, io target) (*Device, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	statePath := filepath.Join(o.RunDir, o.ID)
	b := &backend{store: io, id: o.ID}
	params := &ublk.Params{Backend: b, BlockSize: o.BlockSize, ReadOnly: o.ReadOnly, Recovery: o.Recovery}
	dev, err := pred.recover(o.ID, params)
	recovered := dev != nil
	if dev == nil && err == nil {
		dev, err = ublk.Create(params)
	}
	if err != nil {
		return nil, fmt.Errorf("ublk: %w", err)
	}
	d := &Device{Path: filepath.Join(o.DevDir, o.ID), BlockPath: dev.BlockPath, statePath: statePath, store: store, ublk: dev,
		id: o.ID, base: o.Base, backend: b, started: time.Now(), recovered: recovered}
	if err := os.WriteFile(filepath.Join("/sys/block", filepath.Base(dev.BlockPath), "queue", "read_ahead_kb"), []byte(strconv.Itoa(readAheadKB)), 0); err != nil {
		log.Printf("%s: cannot set read-ahead: %s", o.ID, err.Error())
	}
	if err := d.publish(o.DevDir); err != nil {
		// The caller still owns the store: tear down only what serveStore created
		if d.ownsLink() {
			os.Remove(d.Path)
		}
		dev.Close()
		return nil, err
	}
	if err := announce(dev.BlockPath); err != nil {
		log.Printf("%s: cannot trigger udev: %s", o.ID, err.Error())
	}
	if d.status, err = ListenStatus(filepath.Join(o.RunDir, o.ID+statusSocketExt), d.Status); err != nil {
		log.Printf("%s: no status socket: %s", o.ID, err.Error())
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
		d.hydrator = h
		d.bg.Add(1)
		go func() {
			defer d.bg.Done()
			h.run(bgCtx)
		}()
	}
	return d, nil
}

// defaults fills in the zero-value options.
func (o *Options) defaults() {
	if o.Bitmap == "" {
		o.Bitmap = o.COWFile + config.BitmapExt
	}
	if o.ChunkSize == 0 {
		o.ChunkSize = config.DefaultChunkSize
	}
	if o.BlockSize == 0 {
		o.BlockSize = config.DefaultBlockSize
	}
	if o.DevDir == "" {
		o.DevDir = DevDir
	}
	if o.RunDir == "" {
		o.RunDir = RunDir
	}
}

// Writeback commits the device's overlay into dst, see cow.Store.Writeback. It is for a
// device that is done serving: writes that arrive meanwhile may or may not be included.
func (d *Device) Writeback(dst io.WriterAt) (int64, error) {
	return d.store.Writeback(dst)
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

// predecessor is a kernel device a previous server of this ID left waiting for recovery.
type predecessor struct {
	id    uint32
	valid bool
}

// takeOver deals with the kernel device a previous server of this ID left behind (recorded
// in statePath): a device still served is an error, one waiting for recovery is returned
// for Serve to re-attach, and any other dead one is deleted, since only DEL_DEV removes it.
// A state file whose id now belongs to another server's device is stale and ignored.
func takeOver(o *Options, statePath string) (*predecessor, error) {
	id, pid, ok := readState(statePath)
	if !ok {
		return &predecessor{}, nil
	}
	info, err := ublk.GetInfo(id)
	if err != nil {
		return &predecessor{}, nil
	}
	// A live server is recognized by its pid; a dead one's device by the newest state file
	// claiming its id (the kernel clears the pid of a device waiting for recovery)
	switch alive := info.Live && processAlive(info.ServerPID); {
	case alive && info.ServerPID == os.Getpid():
		// This process served it before re-executing itself (a handoff keeps the pid);
		// Serve already made sure it does not serve it now
		if o.Recovery {
			return &predecessor{id: id, valid: true}, nil
		}
	case alive && (pid == 0 || info.ServerPID == pid):
		return nil, fmt.Errorf("%s is already served by pid %d (%s%d)", o.ID, info.ServerPID, ublkBlockPrefix, id)
	case alive || !ownsKernelID(o.RunDir, o.ID, id):
		return &predecessor{}, nil
	case o.Recovery && (info.Quiesced || info.Live):
		return &predecessor{id: id, valid: true}, nil
	}
	if err := ublk.Delete(id); err != nil && !errors.Is(err, syscall.ENODEV) {
		log.Printf("%s: cannot delete stale ublk device %d: %s", o.ID, id, err.Error())
	} else {
		log.Printf("%s: deleted stale ublk device %d left by a previous server", o.ID, id)
	}
	return &predecessor{}, nil
}

// recover re-attaches to the predecessor; it returns nil without one. A predecessor that
// cannot be taken over (the config changed) is deleted so its waiting I/O fails instead of
// hanging, and the caller creates a fresh device.
func (p *predecessor) recover(id string, params *ublk.Params) (*ublk.Device, error) {
	if !p.valid {
		return nil, nil
	}
	dev, err := ublk.Recover(p.id, params)
	if err == nil {
		p.valid = false // the device is ours now: nothing left to drop
		log.Printf("%s: re-attached to ublk device %d; I/O resumes", id, p.id)
		return dev, nil
	}
	log.Printf("%s: cannot re-attach to ublk device %d: %s; replacing it", id, p.id, err.Error())
	p.drop(id)
	return nil, nil
}

// drop deletes the predecessor, failing its waiting I/O, when it will not be recovered.
func (p *predecessor) drop(id string) {
	if !p.valid {
		return
	}
	p.valid = false
	if err := ublk.Delete(p.id); err != nil && !errors.Is(err, syscall.ENODEV) {
		log.Printf("%s: cannot delete ublk device %d: %s", id, p.id, err.Error())
	}
}

// recoverySupported reports whether the kernel can keep a device across a server's death.
func recoverySupported() bool {
	features, err := ublk.Features()
	return err == nil && features&ublk.FeatureRecovery == ublk.FeatureRecovery
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

// Detach hands the device to a successor process: background work stops and everything
// is made durable, then the caller must exit at once (with ExitDetached under systemd). The
// kernel device keeps waiting I/O until the next Serve of the same ID re-attaches; writes
// served between the flush and the exit are in the live bitmap. Requires Options.Recovery.
func (d *Device) Detach() error {
	defer unmarkServed(d.id)
	if d.stop != nil {
		d.stop()
		d.bg.Wait()
	}
	return d.store.Flush()
}

// Status returns a snapshot of the device's state and counters.
func (d *Device) Status() *Status {
	queues := d.ublk.Stats()
	st := &Status{
		ID:             d.id,
		Path:           d.Path,
		BlockPath:      d.BlockPath,
		PID:            os.Getpid(),
		Started:        d.started,
		Recovered:      d.recovered,
		Size:           d.store.Size(),
		ChunkSize:      d.store.ChunkSize(),
		Chunks:         d.store.Chunks(),
		Written:        d.store.Written(),
		Dirty:          d.store.Dirty(),
		Queues:         queues.Queues,
		ParallelQueues: queues.Parallel,
		IO:             d.backend.stats(),
		Source:         d.store.SourceStats(),
	}
	source.Walk(d.base, func(s source.Source) {
		if c, ok := s.(*source.Cache); ok {
			if st.Cache == nil {
				st.Cache = &source.CacheStats{}
			}
			cs := c.Stats()
			st.Cache.Hits, st.Cache.Misses, st.Cache.Failures = st.Cache.Hits+cs.Hits, st.Cache.Misses+cs.Misses, st.Cache.Failures+cs.Failures
		}
	})
	if d.hydrator != nil {
		p := d.hydrator.progress()
		st.Hydration = &p
	}
	return st
}

// Done is closed if the kernel device fails underneath (a queue thread died); the device
// then answers nothing and should be closed. Err says why.
func (d *Device) Done() <-chan struct{} {
	return d.ublk.Done()
}

// Err returns why the device failed, or nil.
func (d *Device) Err() error {
	return d.ublk.Err()
}

// Close shuts the device down in an order that cannot lose data: background work stops,
// reads blocked in the source are aborted (they fail with EIO; STOP_DEV would otherwise
// wait for them for as long as the source hangs), STOP_DEV drains in-flight I/O (the store
// must still be open for that), the store flushes the COW file and then the bitmap and
// closes, and only then DEL_DEV, which can wait for openers of the block device. A kill
// during that wait finds everything already on disk.
func (d *Device) Close() error {
	if d.closed.Swap(true) {
		return nil
	}
	defer unmarkServed(d.id)
	if d.status != nil {
		d.status.Close()
	}
	if at := mountPoint(d.BlockPath); at != "" {
		log.Printf("%s: still mounted at %s; unmount it, deletion waits for it", filepath.Base(d.Path), at)
	}
	errs := []error{d.halt(), d.store.Close()}
	if err := d.ublk.Delete(); errors.Is(err, ublk.ErrReowned) {
		// A restarted server already owns this id, its symlink and its state file
		log.Printf("%s: kernel device id reused by a successor, leaving its files in place", filepath.Base(d.Path))
		return errors.Join(errs...)
	} else if err != nil {
		errs = append(errs, err)
	}
	// Only unpublish what is still ours: a successor started under the same name may
	// already have replaced the symlink and the state file
	if d.ownsLink() {
		if err := os.Remove(d.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if id, _, ok := readState(d.statePath); ok && id == d.ublk.ID {
		if err := os.Remove(d.statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// halt ends the device's I/O without closing anything: background work stops, reads blocked in
// the source are aborted, and STOP_DEV drains what is in flight. A group halts every device
// before it closes any store, since a device's requests may land in a sibling's store.
// Idempotent.
func (d *Device) halt() error {
	if d.stop != nil {
		d.stop()
		d.bg.Wait()
	}
	d.store.Abort()
	return d.ublk.Stop()
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
// and records the ublk id for takeOver. The link is relative (../ublkbN), the
// form udev writes, so when the udev rule re-creates it the two agree and nothing flaps.
func (d *Device) publish(devDir string) error {
	if err := os.MkdirAll(devDir, devDirMode); err != nil {
		return err
	}
	target, err := filepath.Rel(devDir, d.BlockPath)
	if err != nil {
		return err
	}
	// Replace a stale link atomically, so a mount racing a restart sees either
	tmp := d.Path + ".tmp"
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, d.Path); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(d.statePath), devDirMode); err != nil {
		return err
	}
	return writeState(d.statePath, d.ublk.ID, os.Getpid())
}

// ownsLink reports whether the published symlink still points at this device's kernel
// device, whichever form (relative or absolute) wrote it.
func (d *Device) ownsLink() bool {
	target, err := os.Readlink(d.Path)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(d.Path), target)
	}
	return target == d.BlockPath
}

// markServed records that this process serves id; it reports false if it already does.
func markServed(id string) bool {
	servedMu.Lock()
	defer servedMu.Unlock()
	if served[id] {
		return false
	}
	served[id] = true
	return true
}

// unmarkServed forgets id.
func unmarkServed(id string) {
	servedMu.Lock()
	defer servedMu.Unlock()
	delete(served, id)
}

// Reap disposes of the kernel device a dead server of id left behind. systemd runs it
// (blkmap reap) once it gives up restarting a unit. A device waiting for recovery is
// stopped, which fails its waiting I/O at once; deleting it has to wait until nothing holds
// it open (a mount), so that is left to the next Reap or Serve. A stopped device is deleted.
// Without a recorded device Reap does nothing, and it refuses while the server is alive.
func Reap(id, runDir string) error {
	statePath := filepath.Join(runDir, id)
	kernelID, pid, ok := readState(statePath)
	if !ok {
		return nil
	}
	info, err := ublk.GetInfo(kernelID)
	alive := err == nil && info.Live && processAlive(info.ServerPID)
	switch {
	case errors.Is(err, syscall.ENODEV):
	case err != nil:
		return err
	case alive && (pid == 0 || info.ServerPID == pid):
		return fmt.Errorf("%s is still served by pid %d", id, info.ServerPID)
	case alive || !ownsKernelID(runDir, id, kernelID):
		// The id now belongs to someone else's device; only the state file is stale
	case info.Quiesced || info.Live:
		if err := ublk.Stop(kernelID); err != nil && !errors.Is(err, syscall.ENODEV) {
			return err
		}
		log.Printf("%s: stopped abandoned ublk device %d; its I/O fails now", id, kernelID)
		return nil
	default:
		if err := ublk.Delete(kernelID); err != nil && !errors.Is(err, syscall.ENODEV) {
			return err
		}
		log.Printf("%s: deleted abandoned ublk device %d", id, kernelID)
	}
	if err := os.Remove(statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
