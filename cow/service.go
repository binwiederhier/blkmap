// Package cow layers a copy-on-write overlay over a read-only source. Writes land in a
// sparse COW file at their device offset; a bitmap records which chunks live there.
package cow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"heckel.io/blkmap/source"
)

const (
	// lockStripes bounds the per-chunk mutexes; chunks share a stripe by index modulo.
	lockStripes = 1024
	cowFileMode = 0600
	// MaxRunBytes is the largest HydrateRun served from a pooled buffer; the hydrator sizes
	// its runs to it.
	MaxRunBytes = 1 << 20
	// reclaimSettle is how long Reclaim waits before it looks a second time at a chunk that
	// differed from its base: long enough for the other half of a mirrored write to land.
	reclaimSettle = time.Second
)

var (
	errOutOfRange = errors.New("range beyond device end")
	// ErrSourceChanged means the base no longer has the content the COW file was written
	// over; see Options.Identity.
	ErrSourceChanged = errors.New("source changed")
	// ErrCOWFailed means a sync of the COW file failed: its dirty pages may be gone (Linux
	// marks them clean), so the store refuses all I/O and never commits another bit.
	ErrCOWFailed = errors.New("cow file failed")
)

// Options locates a store's files.
type Options struct {
	COWFile    string
	Bitmap     string
	LiveBitmap string // on tmpfs, optional; see Bitmap
	ChunkSize  int64
	// Identity fingerprints the base's content (source.Identity). It is recorded, and once
	// chunks are written a different one is refused: the overlay belongs to that content.
	// Empty skips the check.
	Identity string
}

// Info is what Inspect reads from a bitmap file.
type Info struct {
	Size      int64 // device size the bitmap was created for
	ChunkSize int64
	Chunks    int64
	Written   int64  // chunks recorded as present in the COW file
	Identity  string // the source the COW file overlays (see Options.Identity)
}

// Store is the writable device image: reads come from the COW file for written chunks and
// from the base source otherwise. It implements ublk.Backend.
type Store struct {
	base        source.Source
	cow         *os.File
	bitmap      *Bitmap
	chunkSize   int64
	size        int64
	dirty       atomic.Bool  // something changed since the last Flush
	nopwrite    atomic.Bool  // drop writes whose bytes equal what the device already reads there
	bufs        sync.Pool    // chunk-sized scratch buffers for read-modify-write
	runBufs     sync.Pool    // MaxRunBytes buffers for hydration runs
	srcReads    atomic.Int64 // base reads, for SourceStats
	srcBytes    atomic.Int64
	demandReads atomic.Int64 // of srcReads, those on the guest read path
	demandBytes atomic.Int64
	onDemand    func(first, count int64) // see OnDemandRead; nil if unset
	srcErrors   atomic.Int64
	srcNanos    atomic.Int64
	syncCOW     func() error  // the COW file's Sync; tests make it fail
	broken      atomic.Bool   // a COW sync failed; failErr says how
	failErr     error         // set once, before broken and failed
	failed      chan struct{} // closed when broken is set
	failOnce    sync.Once
	flushMu     sync.Mutex              // Serializes Flush, whose data-then-bitmap order must not interleave
	locks       [lockStripes]sync.Mutex // Serializes read-modify-write per chunk stripe
	// readMu is held shared by a read of the COW file, from the bitmap test that chose it to the
	// read itself, and exclusively by the punch of a chunk Reclaim dropped (see punchFreed)
	readMu       sync.RWMutex
	readGap      func() // called by a read between its bitmap test and its COW read; tests widen the race there
	writebackGap func() // called by Writeback between its bitmap test and the chunk lock; tests only
	// mutableBase: the base can change under the store (a sibling's view, see
	// source.Durability); the store then relies on its content only where it is durable
	mutableBase bool
	// unsynced has a bit per chunk whose COW data changed since the last data sync, for
	// Durable; allocated by the first Durable call, which a store over this one makes
	unsynced    []uint32
	durOnce     sync.Once
	durTracking atomic.Bool // unsynced and the bitmap's committed copy exist
	durReady    atomic.Bool // a flush completed since tracking began: unsynced is complete
	punchMu     sync.Mutex
	punches     []int64 // chunks Reclaim dropped from the overlay, punched once their cleared bits are on disk
	// recent has one bit per chunk, set when a write changes the chunk's overlay content (or
	// repeats it, see writeChunk) and taken by Reclaim when it examines the chunk. A chunk
	// that differed from its base is collected in again for a second look; due is the set
	// being looked at again, filled from again once empty and examined once dueAt, when it
	// was filled, lies settle back, so every chunk waits at least settle. recentCount counts
	// all three; Reclaim alone touches all but recent (see Reclaim).
	recent, again, due   []uint32
	recentCount          atomic.Int64
	againCount, dueCount int
	recentNext           int64 // the chunk Reclaim scans from next
	dueAt                time.Time
	settle               time.Duration // reclaimSettle, shorter in tests
	reclaimOn            atomic.Bool   // EnableReclaim was called; the sets exist
	reclaimMu            sync.Mutex    // Serializes Reclaim and EnableReclaim
	scanned              bool          // the first Reclaim looked for orphans (see scanOrphans)
	examined             atomic.Int64
	reclaimed            atomic.Int64
	reclaimedBytes       atomic.Int64
}

// Open opens or creates the COW file and bitmap for base. The Store takes ownership of base.
// The COW file is locked so a second server cannot corrupt it, and a bitmap that records
// written chunks refuses a COW file that is missing or shorter than the device: the data it
// describes would read as zeros.
func Open(base source.Source, cowPath, bitmapPath string, chunkSize int64) (*Store, error) {
	return OpenWith(base, &Options{COWFile: cowPath, Bitmap: bitmapPath, ChunkSize: chunkSize})
}

// OpenWith is Open with all options. A server that re-attaches to a device after a crash
// must use a live bitmap, or writes acknowledged before the crash but not yet flushed would
// silently revert.
func OpenWith(base source.Source, o *Options) (*Store, error) {
	cowPath, bitmapPath, livePath, chunkSize := o.COWFile, o.Bitmap, o.LiveBitmap, o.ChunkSize
	size := base.Size()
	cow, err := os.OpenFile(cowPath, os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, cowFileMode)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Store, error) {
		cow.Close()
		return nil, err
	}
	if err := unix.Flock(int(cow.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fail(fmt.Errorf("cow file %s is in use by another process", cowPath))
	}
	st, err := cow.Stat()
	if err != nil {
		return fail(err)
	}
	bitmap, err := OpenLiveBitmap(bitmapPath, livePath, size, chunkSize)
	if err != nil {
		return fail(err)
	}
	if written := bitmap.Count(); written > 0 && st.Size() < size {
		bitmap.CloseNoSync()
		return fail(fmt.Errorf("cow file %s is missing or truncated (%d bytes) but its bitmap records %d written chunks; restore it or delete the bitmap to start over", cowPath, st.Size(), written))
	}
	if err := checkIdentity(bitmap, o.Identity); err != nil {
		bitmap.CloseNoSync()
		return fail(fmt.Errorf("cow file %s: %w", cowPath, err))
	}
	// Extend a fresh COW file to the device size so it is a complete sparse image
	if st.Size() < size {
		if err := cow.Truncate(size); err != nil {
			bitmap.CloseNoSync()
			return fail(fmt.Errorf("cow file %s: %w", cowPath, err))
		}
	}
	s := &Store{base: base, cow: cow, bitmap: bitmap, chunkSize: chunkSize, size: size, mutableBase: source.Mutable(base)}
	s.syncCOW, s.failed = cow.Sync, make(chan struct{})
	s.settle = reclaimSettle
	s.dirty.Store(bitmap.Pending()) // bits adopted from a predecessor's live bitmap
	s.bufs.New = func() any {
		b := make([]byte, chunkSize)
		return &b
	}
	s.runBufs.New = func() any {
		b := make([]byte, MaxRunBytes)
		return &b
	}
	return s, nil
}

// SourceStats counts the reads a store made from its base source.
type SourceStats struct {
	Reads       int64
	Bytes       int64
	Errors      int64
	Duration    time.Duration // total time spent in base reads
	DemandReads int64         // of Reads, those a guest request needed (the chunks were not in the COW file)
	DemandBytes int64
}

// OnDemandRead installs fn, called for every run of chunks a guest read had to fetch from
// the base (first chunk and count) because they were not in the COW file. A hydrator uses it
// to count the listed chunks it was late for. Set before the device serves I/O.
func (s *Store) OnDemandRead(fn func(first, count int64)) {
	s.onDemand = fn
}

// SourceStats returns the base read counters.
func (s *Store) SourceStats() SourceStats {
	return SourceStats{Reads: s.srcReads.Load(), Bytes: s.srcBytes.Load(), Errors: s.srcErrors.Load(), Duration: time.Duration(s.srcNanos.Load()),
		DemandReads: s.demandReads.Load(), DemandBytes: s.demandBytes.Load()}
}

// readBase reads from the base source, counting the read.
func (s *Store) readBase(p []byte, off int64) (int, error) {
	start := time.Now()
	n, err := s.base.ReadAt(p, off)
	s.countBase(n, err, start)
	return n, err
}

// readBaseDirect is readBase bypassing cache tiers (source.ReadDirect).
func (s *Store) readBaseDirect(p []byte, off int64) (int, error) {
	start := time.Now()
	n, err := source.ReadDirect(s.base, p, off)
	s.countBase(n, err, start)
	return n, err
}

func (s *Store) countBase(n int, err error, start time.Time) {
	s.srcReads.Add(1)
	s.srcBytes.Add(int64(n))
	s.srcNanos.Add(int64(time.Since(start)))
	if err != nil && !errors.Is(err, io.EOF) {
		s.srcErrors.Add(1)
	}
}

// checkIdentity records the base's identity, refusing a changed one once chunks are written.
func checkIdentity(bitmap *Bitmap, identity string) error {
	if identity == "" {
		return nil
	}
	recorded, err := bitmap.Identity()
	if err != nil {
		return err
	}
	if recorded == storedIdentity(identity) {
		return nil
	}
	if recorded != "" && bitmap.Count() > 0 {
		return fmt.Errorf("%w: its %d written chunks overlay %q, the source is now %q; restore the original source, or run blkmap pin if the content is the same", ErrSourceChanged, bitmap.Count(), recorded, storedIdentity(identity))
	}
	return bitmap.SetIdentity(identity)
}

func (s *Store) ReadAt(p []byte, off int64) (int, error) {
	if err := s.Err(); err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, fmt.Errorf("%w: negative offset %d", errOutOfRange, off)
	}
	var eof error
	if off >= s.size {
		return 0, io.EOF
	}
	if int64(len(p)) > s.size-off {
		p, eof = p[:s.size-off], io.EOF
	}
	n := len(p)
	// Serve runs of chunks with the same state in one call each, so a request that spans
	// many unwritten chunks reaches the base source once (one round trip for a remote one).
	// The bitmap test and the COW read of a written run happen under readMu, so a chunk
	// Reclaim dropped is not punched in between (it would read as zeros); a base read is not
	// covered, it may block for as long as a remote source takes.
	for len(p) > 0 {
		chunk := off / s.chunkSize
		s.readMu.RLock()
		written := s.bitmap.Test(chunk)
		end := (chunk + 1) * s.chunkSize
		for end < off+int64(len(p)) && s.bitmap.Test(end/s.chunkSize) == written {
			end += s.chunkSize
		}
		m := int(min(int64(len(p)), end-off))
		var read int
		var err error
		if written {
			if s.readGap != nil {
				s.readGap()
			}
			read, err = s.cow.ReadAt(p[:m], off)
		}
		s.readMu.RUnlock()
		if !written {
			read, err = s.readBase(p[:m], off)
			s.demandReads.Add(1)
			s.demandBytes.Add(int64(read))
			if s.onDemand != nil {
				s.onDemand(chunk, (end-1)/s.chunkSize-chunk+1)
			}
		}
		if err := fullRead(read, m, err); err != nil {
			return n - len(p) + read, err
		}
		p = p[m:]
		off += int64(m)
	}
	return n, eof
}

// fullRead turns a ReadAt outcome into an error unless every requested byte arrived: a
// base cut short (a truncated file, a remote source ending early) must never be served as
// data or copied into the overlay, which HydrateRun, ReadAt and writeChunk all rely on.
func fullRead(n, want int, err error) error {
	if n == want && (err == nil || errors.Is(err, io.EOF)) {
		return nil
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return io.ErrUnexpectedEOF
}

func (s *Store) WriteAt(p []byte, off int64) (int, error) {
	if err := s.Err(); err != nil {
		return 0, err
	}
	if off < 0 || off+int64(len(p)) > s.size {
		return 0, fmt.Errorf("%w: offset %d, length %d, size %d", errOutOfRange, off, len(p), s.size)
	}
	n := len(p)
	for len(p) > 0 {
		chunk := off / s.chunkSize
		m := int(min(int64(len(p)), (chunk+1)*s.chunkSize-off))
		if err := s.writeChunk(chunk, p[:m], off); err != nil {
			return n - len(p), err
		}
		p = p[m:]
		off += int64(m)
	}
	return n, nil
}

func (s *Store) Size() int64 {
	return s.size
}

// Abort makes blocked and future base reads fail at once, if the base supports it, so a
// stopping device never waits on a hung source.
func (s *Store) Abort() {
	source.Abort(s.base)
}

// Flush makes all completed writes durable: the bitmap pages are snapshotted first, then the
// COW data is synced, then the snapshot is written, so a bit on disk never describes data
// that is not (a write landing between the two syncs stays dirty for the next Flush). Chunks
// Reclaim dropped from the overlay are punched last, once the snapshot holding their cleared
// bits is on disk: the mirror image of the order for a set bit.
func (s *Store) Flush() error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	if err := s.Err(); err != nil {
		return err
	}
	if !s.dirty.Swap(false) {
		if s.durTracking.Load() {
			s.durReady.Store(true) // nothing changed since the last flush: all of it is synced
		}
		return nil
	}
	punches := s.takePunches() // before the snapshot, so it holds every clear they wait for
	pages := s.bitmap.Snapshot()
	// What changed so far is in the sync below; what changes from here is marked again
	tracking := s.durTracking.Load()
	if tracking {
		for w := range s.unsynced {
			atomic.StoreUint32(&s.unsynced[w], 0)
		}
	}
	if err := s.syncCOW(); err != nil {
		// A retry would find the failed pages clean and succeed without their data
		return s.fail(err)
	}
	if err := s.bitmap.Commit(pages); err != nil {
		s.dirty.Store(true)
		s.queuePunches(punches...)
		return err
	}
	s.punchFreed(punches)
	if tracking {
		s.durReady.Store(true)
	}
	return nil
}

// takePunches hands out the chunks waiting to be punched.
func (s *Store) takePunches() []int64 {
	s.punchMu.Lock()
	defer s.punchMu.Unlock()
	punches := s.punches
	s.punches = nil
	return punches
}

// queuePunches queues chunks to be punched by a Flush, once their cleared bits are durable.
func (s *Store) queuePunches(chunks ...int64) {
	s.punchMu.Lock()
	defer s.punchMu.Unlock()
	s.punches = append(s.punches, chunks...)
}

// punchFreed punches chunks that Reclaim dropped from the overlay, now that the bitmap commit
// is on disk. A chunk is punched only if its bit is clear both in memory and in the file: one
// written meanwhile holds live data again, and one still set in the file (written and dropped
// once more since the snapshot) was queued again by that drop and waits for the next flush.
// Both bits are read under the chunk lock, which every Set and Clear holds. The punch itself
// also excludes readers (readMu): a read that found the bit set and is about to read the COW
// file would otherwise read the hole. A failed punch leaves unreachable data behind, nothing
// worse, so it does not fail the flush.
func (s *Store) punchFreed(punches []int64) {
	for _, chunk := range punches {
		mu := &s.locks[chunk%lockStripes]
		mu.Lock()
		if !s.bitmap.Test(chunk) && !s.bitmap.committed(chunk) {
			s.readMu.Lock()
			s.punch(chunk)
			s.readMu.Unlock()
		}
		mu.Unlock()
	}
}

// Failed is closed once a COW sync failed (see ErrCOWFailed). The store then answers every
// call with Err; close it and start over from the last durable state.
func (s *Store) Failed() <-chan struct{} {
	return s.failed
}

// Err returns why the store failed, or nil.
func (s *Store) Err() error {
	if s.broken.Load() {
		return s.failErr
	}
	return nil
}

// Fail stops the store as a failed COW sync does (see ErrCOWFailed), for an owner that
// finds the COW file unusable some other way. It returns the store's error.
func (s *Store) Fail(err error) error {
	return s.fail(err)
}

// fail stops the store for good and returns why.
func (s *Store) fail(err error) error {
	s.failOnce.Do(func() {
		s.failErr = fmt.Errorf("%w: %w", ErrCOWFailed, err)
		s.broken.Store(true)
		close(s.failed)
	})
	return s.failErr
}

// Written returns the number of chunks that live in the COW file.
func (s *Store) Written() int64 {
	return s.bitmap.Count()
}

// Close flushes and closes the COW file, the bitmap, and the base source. If the data could
// not be made durable, pending bits are dropped rather than written ahead of it; after a
// failed COW sync the live bitmap goes too, since the page cache it vouches for may be gone.
func (s *Store) Close() error {
	err := s.Flush()
	closeBitmap := s.bitmap.Close
	if s.Err() != nil {
		closeBitmap = s.bitmap.CloseDropLive
	} else if err != nil {
		closeBitmap = s.bitmap.CloseNoSync
	}
	return errors.Join(err, s.cow.Close(), closeBitmap(), s.base.Close())
}

// Writeback copies every chunk the overlay holds into dst at its device offset, committing the
// overlay to a writable copy of the base: for a server whose device was a scratch view of
// files that must end up holding the result. It flushes first and leaves the overlay as it is,
// so the device keeps reading what it read before. Chunks that were hole-punched are written
// as zeros; each chunk is read under its lock, so a concurrent write is either in it or not.
// If dst is the base itself, the base's identity changes (see Options.Identity): discard the
// COW file and bitmap afterwards, since the base now holds what they recorded.
func (s *Store) Writeback(dst io.WriterAt) (int64, error) {
	if err := s.Flush(); err != nil { // fails too once the store failed
		return 0, err
	}
	buf := make([]byte, s.chunkSize)
	var n int64
	for chunk := int64(0); chunk < s.bitmap.Chunks(); chunk++ {
		if !s.bitmap.Test(chunk) {
			continue
		}
		if s.writebackGap != nil {
			s.writebackGap()
		}
		start := chunk * s.chunkSize
		length := min(s.chunkSize, s.size-start)
		mu := &s.locks[chunk%lockStripes]
		mu.Lock()
		// Test again under the lock: Reclaim may have dropped the chunk since, and the flush
		// that punches it holds this lock, so the bit decides what the read sees. A dropped
		// chunk equals the base, which the destination already holds
		if !s.bitmap.Test(chunk) {
			mu.Unlock()
			continue
		}
		read, err := s.cow.ReadAt(buf[:length], start)
		mu.Unlock()
		if err := fullRead(read, int(length), err); err != nil {
			return n, fmt.Errorf("read chunk %d from the cow file: %w", chunk, err)
		}
		if _, err := dst.WriteAt(buf[:length], start); err != nil {
			return n, fmt.Errorf("write back chunk %d: %w", chunk, err)
		}
		n++
	}
	return n, nil
}

// SetNopWrite turns nopwrite on or off (the name ZFS uses): a write whose bytes equal what
// the device already returns for that range (from the COW file or the base) is dropped. It
// compares and drops, it never merges. That costs one read per write and saves the copy-up
// and the space for guests that rewrite what is already there, such as a RAID
// resynchronisation after a crash-consistent snapshot.
//
// Over a base derived from sibling devices (a source.Binder: a mirror plex that is a view of
// the other plex, a parity column computed from the data columns) a skipped range keeps
// following the base until a later write there differs. That is what a RAID member does
// anyway, since every write to the array rewrites the dependent member too, and it keeps a
// resynchronisation or parity regeneration from freezing a copy of the whole range in the
// overlay. A chunk that did freeze and later equals its base again is dropped by Reclaim.
func (s *Store) SetNopWrite(on bool) {
	s.nopwrite.Store(on)
}

// writeChunk writes p, which lies entirely within chunk, at device offset off. The first
// write to a chunk that does not cover it entirely first copies the chunk from base, so
// the COW file always holds whole chunks and the bitmap stays exact.
func (s *Store) writeChunk(chunk int64, p []byte, off int64) error {
	start := chunk * s.chunkSize
	length := min(s.chunkSize, s.size-start) // the last chunk may be partial
	mu := &s.locks[chunk%lockStripes]
	mu.Lock()
	defer mu.Unlock()
	written := s.bitmap.Test(chunk)
	partial := int64(len(p)) < length
	nopwrite := s.nopwrite.Load()
	nopBase := nopwrite && !written && s.baseDurable(start, length)
	var buf []byte // the whole chunk from base, when a copy-up or a nopwrite check needs it
	if !written && (partial || nopBase) {
		scratch := s.bufs.Get().(*[]byte)
		defer s.bufs.Put(scratch)
		buf = (*scratch)[:length]
		if read, err := s.readBase(buf, start); fullRead(read, int(length), err) != nil {
			if partial {
				return fmt.Errorf("copy chunk %d from base: %w", chunk, fullRead(read, int(length), err))
			}
			nopBase = false // a whole-chunk write does not need the base: store it
		}
	}
	if nopwrite && (written || nopBase) {
		var same bool
		if written {
			scratch := s.bufs.Get().(*[]byte)
			cur := (*scratch)[:len(p)]
			read, err := s.cow.ReadAt(cur, off)
			same = fullRead(read, len(p), err) == nil && bytes.Equal(cur, p)
			s.bufs.Put(scratch)
		} else {
			// and still durable after the read: a sibling flushed meanwhile still holds the bytes
			same = bytes.Equal(buf[off-start:off-start+int64(len(p))], p) && s.baseDurable(start, length)
		}
		if same {
			if written {
				// The guest repeated the stored bytes; the base may hold them too by now
				s.recordWrite(chunk)
			}
			return nil
		}
	}
	if !written && partial {
		copy(buf[off-start:], p)
		if _, err := s.cow.WriteAt(buf, start); err != nil {
			return err
		}
	} else if _, err := s.cow.WriteAt(p, off); err != nil {
		return err
	}
	s.markUnsynced(chunk)
	s.bitmap.Set(chunk)
	s.dirty.Store(true)
	s.recordWrite(chunk)
	return nil
}

// recordWrite notes for Reclaim that chunk's overlay content changed, or that a write repeated
// it. Callers hold the chunk lock. One atomic or, so the write path stays allocation-free.
func (s *Store) recordWrite(chunk int64) {
	if s.reclaimOn.Load() {
		s.record(s.recent, chunk)
	}
}

// record sets chunk's bit in set, counting it if it was clear, which it reports.
func (s *Store) record(set []uint32, chunk int64) bool {
	mask := uint32(1) << (chunk % bitmapWordBits)
	if atomic.OrUint32(&set[chunk/bitmapWordBits], mask)&mask != 0 {
		return false
	}
	s.recentCount.Add(1)
	return true
}

// EnableReclaim turns Reclaim on: from now on writes are recorded for it. Its state (three
// chunk sets and the bitmap's copy of the file) costs about four bits per chunk, so a store
// allocates it only here. Call it before the store serves I/O, or writes before it are never
// examined.
func (s *Store) EnableReclaim() {
	s.reclaimMu.Lock()
	defer s.reclaimMu.Unlock()
	if s.reclaimOn.Load() {
		return
	}
	words := (s.bitmap.Chunks() + bitmapWordBits - 1) / bitmapWordBits
	s.recent, s.again, s.due = make([]uint32, words), make([]uint32, words), make([]uint32, words)
	s.bitmap.trackCommitted()
	s.reclaimOn.Store(true)
}

// Durable reports whether the device's content in [off, off+length) would read the same after
// a crash (source.Durability), for a store whose base is a view of this one: a written chunk
// is durable once its bit is on disk and its data synced since its last change, an unwritten
// one once its clear is on disk and its base is durable. The first call starts the tracking
// it needs (a bit per chunk) and answers false until a flush has completed, since changes
// before it were not tracked. Must not allocate after the first call.
func (s *Store) Durable(off, length int64) bool {
	s.durOnce.Do(s.trackDurability)
	if !s.durReady.Load() || s.Err() != nil {
		return false
	}
	end := min(off+length, s.size)
	if off < 0 || off >= end {
		return true
	}
	unwritten := false
	for c := off / s.chunkSize; c*s.chunkSize < end; c++ {
		if s.bitmap.Test(c) {
			if !s.bitmap.committed(c) || atomic.LoadUint32(&s.unsynced[c/bitmapWordBits])&(1<<(c%bitmapWordBits)) != 0 {
				return false
			}
		} else if s.bitmap.committed(c) {
			return false // its clear is not on disk yet: a crash would bring the old copy back
		} else {
			unwritten = true
		}
	}
	return !unwritten || s.baseDurable(off, end-off)
}

// TrackDurability starts what Durable needs, for a store that other stores' bases read (a
// device group's member). Called before the store serves I/O, Durable is exact at once: the
// data behind every committed bit is synced, and bits adopted from a live bitmap are not
// committed yet. Called later (or not at all), Durable answers false until the next flush.
func (s *Store) TrackDurability() {
	fresh := false
	s.durOnce.Do(func() {
		s.trackDurability()
		fresh = true
	})
	if fresh && !s.dirty.Load() {
		s.durReady.Store(true)
	}
}

// trackDurability starts what Durable needs: the unsynced chunks and the committed bits.
func (s *Store) trackDurability() {
	s.unsynced = make([]uint32, (s.bitmap.Chunks()+bitmapWordBits-1)/bitmapWordBits)
	s.bitmap.trackCommitted()
	s.durTracking.Store(true)
}

// markUnsynced notes that chunk's COW data changed; callers hold the chunk lock.
func (s *Store) markUnsynced(chunk int64) {
	if s.durTracking.Load() {
		atomic.OrUint32(&s.unsynced[chunk/bitmapWordBits], 1<<(chunk%bitmapWordBits))
	}
}

// baseDurable reports whether the base's content in [off, off+length) is durable: always for
// read-only content, as source.Durable answers for a base that can change. The store skips a
// write or drops a chunk in favour of its base only where it is, or a crash of a sibling could
// take the only copy of data this store already made durable.
func (s *Store) baseDurable(off, length int64) bool {
	return !s.mutableBase || source.Durable(s.base, off, length)
}

// ReclaimStats counts what Reclaim did and has left to do.
type ReclaimStats struct {
	Pending  int64 `json:"pending"`  // chunks waiting to be examined, or examined again
	Examined int64 `json:"examined"` // chunks compared with their base so far
	Chunks   int64 `json:"chunks"`   // chunks dropped from the overlay
	Bytes    int64 `json:"bytes"`
}

// ReclaimStats returns the Reclaim counters.
func (s *Store) ReclaimStats() ReclaimStats {
	return ReclaimStats{Pending: s.recentCount.Load(), Examined: s.examined.Load(), Chunks: s.reclaimed.Load(), Bytes: s.reclaimedBytes.Load()}
}

// Reclaim examines up to budget chunks written since it last examined them and drops from the
// overlay every one that holds exactly what its base reads now: the bit is cleared, so reads
// follow the base again, and the chunk is queued to be punched by the Flush that puts the
// cleared bit on disk. It reports how many chunks it examined; 0 means none was waiting.
//
// It exists for nopwrite over a derived base (see SetNopWrite). A chunk freezes in the overlay
// the first time a write to it differs from the base at that instant, and over a mirror plex
// that happens by timing alone: the halves of a mirrored write land in either order, and the
// plex whose half comes first sees the other still holding the old bytes. A moment later the
// plexes agree again and the stored chunk is a redundant copy of its base, but every later
// write to it is compared against that copy instead of the base, so a hot chunk stays stored
// on both plexes for good. Reclaim compares later, once the other half has landed, and undoes
// the freeze; a parity chunk written before its data chunks thaws the same way.
//
// Each examined chunk costs one overlay read and one base read under its chunk lock; a base
// that cannot be read leaves the chunk as it is. A chunk that differs is looked at once more
// after a settle time (a second, so the other half of a mirrored write has landed; an
// examination between the halves would otherwise miss a chunk the guest never touches again)
// and then forgotten until a write records it again. A write that repeats a stored chunk's
// bytes records it too, with nopwrite: the guest touching the chunk is the sign that its
// sibling got the same bytes. Chunks hydration copies are never recorded: they duplicate the
// base on purpose. The base must be the content the overlay was written over; a stand-in for
// a complete overlay (a zero source) would absorb chunks that happen to read like it. The
// punch waits for the flush because it could otherwise reach the disk before the cleared bit
// does, and a set bit on disk over a punched chunk would read zeros after a crash, while a
// cleared bit over stale data reads the base, which holds the same bytes. A predecessor that
// died owing punches (its queue was process memory) left the chunks allocated: the first call
// finds them in the COW file and queues them. Calls are serialized.
func (s *Store) Reclaim(ctx context.Context, budget int) int {
	s.reclaimMu.Lock()
	defer s.reclaimMu.Unlock()
	if !s.reclaimOn.Load() || s.Err() != nil {
		return 0
	}
	if !s.scanned {
		s.scanned = true
		s.scanOrphans()
	}
	if s.recentCount.Load() == 0 {
		return 0
	}
	stored, base := s.bufs.Get().(*[]byte), s.bufs.Get().(*[]byte)
	defer s.bufs.Put(stored)
	defer s.bufs.Put(base)
	examined := 0
	for examined < budget && ctx.Err() == nil {
		chunk, second, ok := s.nextRecent()
		if !ok {
			break
		}
		examined++
		freed, retry := s.reclaimChunk(chunk, *stored, *base)
		if freed {
			s.forget(chunk)
		} else if (retry || !second) && s.bitmap.Test(chunk) && s.record(s.again, chunk) {
			// A chunk kept for want of a durable base keeps its turns until the sibling flushes
			s.againCount++
		}
	}
	s.examined.Add(int64(examined))
	return examined
}

// nextRecent takes the first recorded chunk at or after the scan position and moves the
// position past it, wrapping around once. A chunk recorded behind the position (one written
// again right after it was examined) waits for the wrap, so a hot chunk cannot starve the
// rest. When nothing is recorded it takes a chunk due for its second look, if the set waiting
// for one has settled (second is then true); false when there is nothing to examine. The
// caller holds reclaimMu.
func (s *Store) nextRecent() (chunk int64, second, ok bool) {
	words := int64(len(s.recent))
	for n := int64(0); n <= words; n++ {
		w := (s.recentNext/bitmapWordBits + n) % words
		word := atomic.LoadUint32(&s.recent[w])
		if n == 0 {
			word &= ^uint32(0) << (s.recentNext % bitmapWordBits) // only chunks at or after the position
		}
		if word == 0 {
			continue
		}
		bit := bits.TrailingZeros32(word)
		atomic.AndUint32(&s.recent[w], ^(uint32(1) << bit))
		s.recentCount.Add(-1)
		chunk = w*bitmapWordBits + int64(bit)
		s.recentNext = (chunk + 1) % (words * bitmapWordBits)
		return chunk, false, true
	}
	if chunk, ok := s.nextDue(); ok {
		return chunk, true, true
	}
	return 0, false, false
}

// scanOrphans queues every allocated chunk of the COW file whose bit is clear: a chunk a
// predecessor dropped whose punch never happened (the crash came before the flush, or between
// the bitmap commit and the punch), so the next flush punches it. Allocation is read with
// SEEK_DATA, a few calls per extent; a filesystem without it reports nothing. The bits are
// read without the chunk locks, the flush checks them again under those (see punchFreed).
func (s *Store) scanOrphans() {
	fd := int(s.cow.Fd())
	var orphans []int64
	for pos := int64(0); pos < s.size; {
		data, err := unix.Seek(fd, pos, unix.SEEK_DATA)
		if err != nil || data >= s.size { // ENXIO: no data up to the end
			break
		}
		hole, err := unix.Seek(fd, data, unix.SEEK_HOLE)
		if err != nil {
			break
		}
		for chunk := data / s.chunkSize; chunk*s.chunkSize < min(hole, s.size); chunk++ {
			if !s.bitmap.Test(chunk) {
				orphans = append(orphans, chunk)
			}
		}
		pos = hole
	}
	if len(orphans) > 0 {
		s.queuePunches(orphans...)
		s.dirty.Store(true)
	}
}

// forget drops a chunk Reclaim just freed from the sets waiting for a second look.
func (s *Store) forget(chunk int64) {
	mask := uint32(1) << (chunk % bitmapWordBits)
	for _, set := range []struct {
		bits  []uint32
		count *int
	}{{s.again, &s.againCount}, {s.due, &s.dueCount}} {
		if set.bits[chunk/bitmapWordBits]&mask != 0 {
			set.bits[chunk/bitmapWordBits] &^= mask
			*set.count--
			s.recentCount.Add(-1)
		}
	}
}

// nextDue takes the lowest chunk due for a second look. An empty due set is refilled from
// again and then left alone for settle, so every chunk waits at least that long, and nothing
// is added to a due set, so no chunk is looked at a third time. The caller holds reclaimMu.
func (s *Store) nextDue() (int64, bool) {
	if s.dueCount == 0 {
		if s.againCount == 0 {
			return 0, false
		}
		s.due, s.again, s.dueCount, s.againCount, s.dueAt = s.again, s.due, s.againCount, 0, time.Now()
	}
	if time.Since(s.dueAt) < s.settle {
		return 0, false
	}
	for w := range s.due {
		if word := s.due[w]; word != 0 {
			bit := bits.TrailingZeros32(word)
			s.due[w] &^= uint32(1) << bit
			s.dueCount--
			s.recentCount.Add(-1)
			return int64(w)*bitmapWordBits + int64(bit), true
		}
	}
	return 0, false
}

// reclaimChunk drops chunk from the overlay if it holds exactly what its base reads now and
// that content is durable (see Reclaim); stored and base are chunk-sized scratch buffers.
// retry reports a chunk kept only because its base is not durable yet: a sibling that has not
// flushed the same bytes; it is worth another look later.
func (s *Store) reclaimChunk(chunk int64, stored, base []byte) (freed, retry bool) {
	start := chunk * s.chunkSize
	length := min(s.chunkSize, s.size-start)
	stored, base = stored[:length], base[:length]
	mu := &s.locks[chunk%lockStripes]
	mu.Lock()
	defer mu.Unlock()
	if !s.bitmap.Test(chunk) {
		return false, false
	}
	if !s.baseDurable(start, length) {
		return false, true
	}
	if read, err := s.cow.ReadAt(stored, start); fullRead(read, int(length), err) != nil {
		return false, false
	}
	if read, err := s.readBase(base, start); fullRead(read, int(length), err) != nil || !bytes.Equal(stored, base) {
		return false, false
	}
	if !s.baseDurable(start, length) { // again after the read: what it read is what is durable
		return false, true
	}
	s.bitmap.Clear(chunk)
	s.queuePunches(chunk)
	s.dirty.Store(true)
	s.reclaimed.Add(1)
	s.reclaimedBytes.Add(length)
	return true, false
}

// Discard drops a range: whole chunks already in the COW file are punched out, so they read
// as zeros and stop using space. Partial chunks and unwritten chunks are left alone, which
// discard semantics allow.
func (s *Store) Discard(off, length int64) error {
	if err := s.Err(); err != nil {
		return err
	}
	if err := s.checkRange(off, length); err != nil {
		return err
	}
	first := (off + s.chunkSize - 1) / s.chunkSize
	end := (off + length) / s.chunkSize
	for chunk := first; chunk < end; chunk++ {
		mu := &s.locks[chunk%lockStripes]
		mu.Lock()
		var err error
		if s.bitmap.Test(chunk) {
			err = s.punch(chunk)
			s.dirty.Store(true)
			s.recordWrite(chunk)
		}
		mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

// WriteZeroes zeroes a range: whole chunks are punched out and marked written (reading as
// zeros from the sparse COW file); partial chunks go through the normal write path.
func (s *Store) WriteZeroes(off, length int64) error {
	if err := s.Err(); err != nil {
		return err
	}
	if err := s.checkRange(off, length); err != nil {
		return err
	}
	var zeros []byte
	for end := off + length; off < end; {
		chunk := off / s.chunkSize
		chunkStart := chunk * s.chunkSize
		chunkEnd := min(chunkStart+s.chunkSize, s.size)
		m := min(end, chunkEnd) - off
		var err error
		if off == chunkStart && m == chunkEnd-chunkStart {
			mu := &s.locks[chunk%lockStripes]
			mu.Lock()
			if s.nopwrite.Load() && !s.bitmap.Test(chunk) && s.baseDurable(chunkStart, m) && s.baseIsZero(chunkStart, m) && s.baseDurable(chunkStart, m) {
				// the base already reads as zeros there: nothing to record
			} else if err = s.punch(chunk); err == nil {
				s.bitmap.Set(chunk)
				s.dirty.Store(true)
				s.recordWrite(chunk)
			}
			mu.Unlock()
		} else {
			if int64(len(zeros)) < m {
				zeros = make([]byte, m)
			}
			err = s.writeChunk(chunk, zeros[:m], off)
		}
		if err != nil {
			return err
		}
		off += m
	}
	return nil
}

// baseIsZero reports whether the base reads as zeros over [start, start+length): from its
// hole map when it has one, else by reading it. Callers hold the chunk lock.
func (s *Store) baseIsZero(start, length int64) bool {
	if holes, err := source.Holes(s.base, start, length); err == nil && len(holes) == 1 && holes[0].Offset == start && holes[0].Length == length {
		return true
	}
	scratch := s.bufs.Get().(*[]byte)
	defer s.bufs.Put(scratch)
	buf := (*scratch)[:length]
	if _, err := s.base.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	for _, b := range buf {
		if b != 0 {
			return false
		}
	}
	return true
}

// punch deallocates chunk in the COW file; it reads back as zeros. Callers hold the lock.
func (s *Store) punch(chunk int64) error {
	s.markUnsynced(chunk)
	start := chunk * s.chunkSize
	length := min(s.chunkSize, s.size-start)
	return unix.Fallocate(int(s.cow.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, start, length)
}

func (s *Store) checkRange(off, length int64) error {
	if off < 0 || length < 0 || off+length > s.size {
		return fmt.Errorf("%w: offset %d, length %d, size %d", errOutOfRange, off, length, s.size)
	}
	return nil
}

// Chunks returns the number of chunks in the device.
func (s *Store) Chunks() int64 {
	return s.bitmap.Chunks()
}

// ChunkSize returns the COW granularity in bytes.
func (s *Store) ChunkSize() int64 {
	return s.chunkSize
}

// IsWritten reports whether chunk lives in the COW file; out-of-range chunks do not.
func (s *Store) IsWritten(chunk int64) bool {
	return s.bitmap.Test(chunk)
}

// MarkZero records chunk as written without copying anything, for chunks known to read as
// zeros. It reports whether the bit was newly set. The chunk is punched first: after a crash
// the COW file can still hold data whose bit never reached the disk.
func (s *Store) MarkZero(chunk int64) bool {
	if chunk < 0 || chunk >= s.bitmap.Chunks() || s.Err() != nil {
		return false
	}
	mu := &s.locks[chunk%lockStripes]
	mu.Lock()
	defer mu.Unlock()
	if s.bitmap.Test(chunk) || s.punch(chunk) != nil {
		return false
	}
	s.bitmap.Set(chunk)
	s.dirty.Store(true)
	return true
}

// Stat returns the COW file's metadata (its allocated blocks show how sparse it is).
func (s *Store) Stat() (*syscall.Stat_t, error) {
	fi, err := s.cow.Stat()
	if err != nil {
		return nil, err
	}
	return fi.Sys().(*syscall.Stat_t), nil
}

// Complete reports, from the bitmap file alone, whether every chunk has been written, along
// with the device size and chunk size the bitmap was created for. A missing bitmap is not an
// error: complete is false.
func Complete(bitmapPath string) (size, chunkSize int64, complete bool, err error) {
	info, err := Inspect(bitmapPath)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return info.Size, info.ChunkSize, info.Written == info.Chunks, nil
}

// Inspect reads a bitmap file without opening the store.
func Inspect(bitmapPath string) (*Info, error) {
	data, err := os.ReadFile(bitmapPath)
	if err != nil {
		return nil, err
	}
	size, chunkSize, err := parseHeader(data)
	if err != nil {
		return nil, fmt.Errorf("%w %s: %w", errBitmap, bitmapPath, err)
	}
	info := &Info{Size: size, ChunkSize: chunkSize, Chunks: (size + chunkSize - 1) / chunkSize, Identity: headerIdentity(data)}
	if int64(len(data)-bitmapHeaderSize)*8 < info.Chunks {
		return nil, fmt.Errorf("%w %s: file too short for %d chunks", errBitmap, bitmapPath, info.Chunks)
	}
	for _, b := range data[bitmapHeaderSize:] {
		info.Written += int64(bits.OnesCount8(b))
	}
	return info, nil
}

// Dirty reports whether anything changed since the last Flush.
func (s *Store) Dirty() bool {
	return s.dirty.Load()
}

// HydrateRun copies the unwritten chunks among [first, first+count) from the base with one
// read, so a remote source sees one request per run instead of one per chunk. It reports the
// bytes copied; chunks the guest wrote meanwhile keep the guest's data. Hydrated chunks are
// not recorded for Reclaim (nor are MarkZero's): they duplicate the base on purpose.
func (s *Store) HydrateRun(first, count int64, direct bool) (int64, error) {
	if err := s.Err(); err != nil {
		return 0, err
	}
	chunks := s.bitmap.Chunks()
	if first < 0 || first >= chunks || count <= 0 {
		return 0, fmt.Errorf("%w: chunks %d..%d of %d", errOutOfRange, first, first+count, chunks)
	}
	last := min(first+count, chunks)
	start := first * s.chunkSize
	length := min(last*s.chunkSize, s.size) - start
	var buf []byte
	if length <= MaxRunBytes {
		scratch := s.runBufs.Get().(*[]byte)
		defer s.runBufs.Put(scratch)
		buf = (*scratch)[:length]
	} else {
		buf = make([]byte, length)
	}
	var n int
	var err error
	if direct {
		n, err = s.readBaseDirect(buf, start)
	} else {
		n, err = s.readBase(buf, start)
	}
	if err != nil && !(errors.Is(err, io.EOF) && n == len(buf)) {
		return 0, fmt.Errorf("hydrate chunks %d..%d: %w", first, last, err)
	}
	if n < len(buf) {
		return 0, fmt.Errorf("hydrate chunks %d..%d: short read (%d of %d bytes)", first, last, n, len(buf))
	}
	var copied int64
	for chunk := first; chunk < last; chunk++ {
		off := (chunk - first) * s.chunkSize
		data := buf[off:min(off+s.chunkSize, int64(len(buf)))]
		mu := &s.locks[chunk%lockStripes]
		mu.Lock()
		if !s.bitmap.Test(chunk) {
			if _, err := s.cow.WriteAt(data, start+off); err != nil {
				mu.Unlock()
				return copied, err
			}
			s.markUnsynced(chunk)
			s.bitmap.Set(chunk)
			s.dirty.Store(true)
			copied += int64(len(data))
		}
		mu.Unlock()
	}
	return copied, nil
}
