package cow

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	// bitmapHeaderSize keeps the bit area page-aligned.
	bitmapHeaderSize = 4096
	bitmapMagic      = "BLKMAPBM"
	bitmapVersion    = 1
	// Header layout: magic[8] version[4] pad[4] size[8] chunk[8] identity-len[4] identity[...]
	bitmapOffVersion  = 8
	bitmapOffSize     = 16
	bitmapOffChunk    = 24
	bitmapOffIdentity = 32
	// bitmapIdentityMax bounds the stored identity; longer ones are stored as a hash.
	bitmapIdentityMax  = 3072
	identityHashPrefix = "sha256:"
	// bitmapWordBits is the width of the atomic word the bit area is made of.
	bitmapWordBits = 32
	bitmapWordSize = bitmapWordBits / 8
	// bitmapPageSize is the write-out unit: Sync rewrites only pages with changed bits.
	bitmapPageSize = 4096
	bitmapFileMode = 0600
)

var (
	errBitmap = errors.New("bitmap")
)

// Bitmap is a persistent bit-per-chunk map recording which chunks live in the COW file. The
// bits live in memory and reach the file only in Sync, which the store calls after the COW
// data is on disk, so a bit on disk always has its data on disk. A small header pins the
// geometry so a stale or foreign bitmap is rejected.
//
// With a live file (on tmpfs), the in-memory bits are a shared mapping of that file instead
// of process memory. It outlives a crash of the process but not a reboot, exactly like the
// COW file's page cache, so a restarted server knows every chunk whose write was
// acknowledged, flushed or not. Without it, a server re-attaching to a still-mounted device
// would silently revert those writes.
type Bitmap struct {
	f        *os.File
	words    []uint32
	chunks   int64
	dirty    []atomic.Bool // one per bitmapPageSize of the bit area
	live     *os.File      // nil without a live file
	livePath string
	liveMap  []byte
	syncMu   sync.Mutex // Serializes Sync
}

// OpenBitmap opens or creates the bitmap for a device of size bytes and the given chunk size.
func OpenBitmap(path string, size, chunkSize int64) (*Bitmap, error) {
	return OpenLiveBitmap(path, "", size, chunkSize)
}

// OpenLiveBitmap is OpenBitmap with a live file at livePath (see Bitmap). A live file left
// by a crashed predecessor with the same geometry is adopted; its bits are a superset of the
// file's, since bits only ever reach the file after the live map.
func OpenLiveBitmap(path, livePath string, size, chunkSize int64) (*Bitmap, error) {
	chunks := (size + chunkSize - 1) / chunkSize
	words := (chunks + bitmapWordBits - 1) / bitmapWordBits
	areaSize := (words*bitmapWordSize + bitmapPageSize - 1) / bitmapPageSize * bitmapPageSize
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, bitmapFileMode)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.Size() == 0 {
		err = writeHeader(f, bitmapHeaderSize+areaSize, size, chunkSize)
		// Allocate the whole file now: a full cow filesystem must not stop the bitmap from
		// recording what made it into the cow file
		if err == nil {
			err = preallocate(f, bitmapHeaderSize+areaSize)
		}
	} else {
		err = checkHeader(f, size, chunkSize)
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%w %s: %w", errBitmap, path, err)
	}
	b := &Bitmap{f: f, words: make([]uint32, areaSize/bitmapWordSize), chunks: chunks, dirty: make([]atomic.Bool, areaSize/bitmapPageSize)}
	if _, err := f.ReadAt(b.area(), bitmapHeaderSize); err != nil {
		f.Close()
		return nil, fmt.Errorf("%w %s: read: %w", errBitmap, path, err)
	}
	if livePath != "" {
		if err := b.attachLive(livePath, size, chunkSize); err != nil {
			f.Close()
			return nil, fmt.Errorf("%w %s: live bitmap %s: %w", errBitmap, path, livePath, err)
		}
	}
	return b, nil
}

// attachLive moves the bits into a shared mapping of the live file, adopting the file's
// bits if a predecessor left it with this geometry. Pages that differ from the disk file
// are marked dirty, so the next Sync persists what the predecessor never did.
func (b *Bitmap) attachLive(path string, size, chunkSize int64) error {
	fileSize := int64(bitmapHeaderSize + len(b.area()))
	live, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, bitmapFileMode)
	if err != nil {
		return err
	}
	st, err := live.Stat()
	if err != nil {
		live.Close()
		return err
	}
	adopt := st.Size() == fileSize && checkHeader(live, size, chunkSize) == nil
	if !adopt {
		if err := live.Truncate(0); err != nil {
			live.Close()
			return err
		}
		if err := writeHeader(live, fileSize, size, chunkSize); err != nil {
			live.Close()
			return err
		}
	}
	m, err := unix.Mmap(int(live.Fd()), 0, int(fileSize), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		live.Close()
		return err
	}
	disk := b.area()
	area := m[bitmapHeaderSize:]
	if adopt {
		for i := 0; i < len(area); i += bitmapPageSize {
			if !bytes.Equal(area[i:i+bitmapPageSize], disk[i:i+bitmapPageSize]) {
				b.dirty[i/bitmapPageSize].Store(true)
			}
		}
	} else {
		copy(area, disk)
	}
	b.live, b.livePath, b.liveMap = live, path, m
	b.words = unsafe.Slice((*uint32)(unsafe.Pointer(unsafe.SliceData(area))), len(area)/bitmapWordSize)
	return nil
}

// Identity returns the source identity recorded in the header ("" if none).
func (b *Bitmap) Identity() (string, error) {
	header := make([]byte, bitmapHeaderSize)
	if _, err := b.f.ReadAt(header, 0); err != nil {
		return "", err
	}
	return headerIdentity(header), nil
}

// SetIdentity records the source identity in the header, durably.
func (b *Bitmap) SetIdentity(identity string) error {
	return writeIdentity(b.f, identity)
}

// Pin records identity as the source of a COW file's bitmap, accepting a source whose
// identity changed but whose content the operator knows to be the same. The COW file must
// not be in use by a server.
func Pin(cowPath, path, identity string) error {
	cow, err := os.OpenFile(cowPath, os.O_RDWR|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer cow.Close()
	if err := unix.Flock(int(cow.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("cow file %s is in use; stop the device first", cowPath)
	}
	f, err := os.OpenFile(path, os.O_RDWR|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	header := make([]byte, bitmapHeaderSize)
	if _, err := f.ReadAt(header, 0); err != nil {
		return fmt.Errorf("%w %s: %w", errBitmap, path, err)
	}
	if _, _, err := parseHeader(header); err != nil {
		return fmt.Errorf("%w %s: %w", errBitmap, path, err)
	}
	return writeIdentity(f, identity)
}

// storedIdentity is the form identity takes in the header: itself, or a hash if too long.
func storedIdentity(identity string) string {
	if len(identity) <= bitmapIdentityMax {
		return identity
	}
	sum := sha256.Sum256([]byte(identity))
	return identityHashPrefix + hex.EncodeToString(sum[:])
}

// writeIdentity writes the identity field of the header and makes it durable.
func writeIdentity(f *os.File, identity string) error {
	stored := storedIdentity(identity)
	field := make([]byte, 4+bitmapIdentityMax)
	binary.LittleEndian.PutUint32(field, uint32(len(stored)))
	copy(field[4:], stored)
	if _, err := f.WriteAt(field, bitmapOffIdentity); err != nil {
		return err
	}
	return f.Sync()
}

// headerIdentity reads the identity field of a header.
func headerIdentity(header []byte) string {
	n := binary.LittleEndian.Uint32(header[bitmapOffIdentity:])
	if n == 0 || n > bitmapIdentityMax {
		return ""
	}
	return string(header[bitmapOffIdentity+4 : bitmapOffIdentity+4+int(n)])
}

// abandon releases the bitmap without syncing, like a killed process; the live file stays.
func (b *Bitmap) abandon() {
	b.releaseLive(false)
	b.f.Close()
}

// Test reports whether chunk i is marked written; out-of-range chunks are not.
func (b *Bitmap) Test(i int64) bool {
	if i < 0 || i >= b.chunks {
		return false
	}
	return atomic.LoadUint32(&b.words[i/bitmapWordBits])&(1<<(i%bitmapWordBits)) != 0
}

// Set marks chunk i as written. The bit is in memory until the next Sync.
func (b *Bitmap) Set(i int64) {
	if i < 0 || i >= b.chunks {
		return
	}
	word := i / bitmapWordBits
	atomic.OrUint32(&b.words[word], 1<<(i%bitmapWordBits))
	b.dirty[word*bitmapWordSize/bitmapPageSize].Store(true)
}

// Pending reports whether any bits are not yet in the file.
func (b *Bitmap) Pending() bool {
	for i := range b.dirty {
		if b.dirty[i].Load() {
			return true
		}
	}
	return false
}

// Count returns the number of chunks marked written.
func (b *Bitmap) Count() int64 {
	var n int64
	for i := range b.words {
		n += int64(bits.OnesCount32(atomic.LoadUint32(&b.words[i])))
	}
	return n
}

// Chunks returns the number of chunks the bitmap covers.
func (b *Bitmap) Chunks() int64 {
	return b.chunks
}

// page is a snapshot of one dirty page of the bit area, taken before the COW data is synced
// so that only bits whose data is already durable can reach the file.
type page struct {
	index int
	data  []byte
}

// Snapshot copies every page with changed bits and clears their dirty flags. Bits set
// afterwards stay dirty for the next snapshot.
func (b *Bitmap) Snapshot() []page {
	var pages []page
	area := b.area()
	for i := range b.dirty {
		if !b.dirty[i].Swap(false) {
			continue
		}
		start := i * bitmapPageSize
		pages = append(pages, page{index: i, data: append([]byte(nil), area[start:start+bitmapPageSize]...)})
	}
	return pages
}

// Commit writes snapshotted pages to the file and makes it durable. On failure the pages are
// marked dirty again so a later Sync retries them.
func (b *Bitmap) Commit(pages []page) error {
	if len(pages) == 0 {
		return nil
	}
	b.syncMu.Lock()
	defer b.syncMu.Unlock()
	for _, p := range pages {
		if _, err := b.f.WriteAt(p.data, int64(bitmapHeaderSize+p.index*bitmapPageSize)); err != nil {
			b.Redirty(pages)
			return err
		}
	}
	if err := b.f.Sync(); err != nil {
		b.Redirty(pages)
		return err
	}
	return nil
}

// Redirty marks snapshotted pages dirty again, when their data sync failed.
func (b *Bitmap) Redirty(pages []page) {
	for _, p := range pages {
		b.dirty[p.index].Store(true)
	}
}

// Sync writes every page with changed bits to the file and makes the file durable. Call it
// only when the data those bits describe is already durable; the store uses Snapshot and
// Commit around its own data sync instead.
func (b *Bitmap) Sync() error {
	return b.Commit(b.Snapshot())
}

// Close syncs and closes the bitmap. Once everything is on disk the live file is removed:
// the disk file is authoritative again.
func (b *Bitmap) Close() error {
	err := b.Sync()
	return errors.Join(err, b.releaseLive(err == nil), b.f.Close())
}

// CloseNoSync closes the bitmap without writing pending bits, for when their data is not
// known to be durable. The live file stays: it still describes the cow file's page cache.
func (b *Bitmap) CloseNoSync() error {
	return errors.Join(b.releaseLive(false), b.f.Close())
}

// releaseLive unmaps and closes the live file, removing it if remove is set.
func (b *Bitmap) releaseLive(remove bool) error {
	if b.live == nil {
		return nil
	}
	// The words alias the mapping; keep a private copy so late readers stay valid
	words := append([]uint32(nil), b.words...)
	b.words = words
	errs := []error{unix.Munmap(b.liveMap), b.live.Close()}
	if remove {
		errs = append(errs, os.Remove(b.livePath))
	}
	b.live, b.liveMap = nil, nil
	return errors.Join(errs...)
}

// area views the words as the bytes stored in the file (host byte order).
func (b *Bitmap) area() []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(unsafe.SliceData(b.words))), len(b.words)*bitmapWordSize)
}

// writeHeader initializes a fresh bitmap file: header plus a zeroed (sparse) bit area.
func writeHeader(f *os.File, fileSize, size, chunkSize int64) error {
	header := make([]byte, bitmapHeaderSize)
	copy(header, bitmapMagic)
	binary.LittleEndian.PutUint32(header[bitmapOffVersion:], bitmapVersion)
	binary.LittleEndian.PutUint64(header[bitmapOffSize:], uint64(size))
	binary.LittleEndian.PutUint64(header[bitmapOffChunk:], uint64(chunkSize))
	if _, err := f.WriteAt(header, 0); err != nil {
		return err
	}
	if err := f.Truncate(fileSize); err != nil {
		return err
	}
	return f.Sync()
}

// checkHeader verifies an existing bitmap file belongs to a device of this geometry.
func checkHeader(f *os.File, size, chunkSize int64) error {
	header := make([]byte, bitmapHeaderSize)
	if _, err := f.ReadAt(header, 0); err != nil {
		return fmt.Errorf("not a blkmap bitmap: %w", err)
	}
	s, c, err := parseHeader(header)
	if err != nil {
		return err
	}
	if s != size {
		return fmt.Errorf("device size mismatch: bitmap was created for %d bytes, device is %d", s, size)
	}
	if c != chunkSize {
		return fmt.Errorf("chunk size mismatch: bitmap was created with %d, config says %d", c, chunkSize)
	}
	return nil
}

// parseHeader validates the magic, version and geometry and returns the recorded geometry.
func parseHeader(header []byte) (size, chunkSize int64, err error) {
	if len(header) < bitmapHeaderSize || string(header[:len(bitmapMagic)]) != bitmapMagic {
		return 0, 0, errors.New("not a blkmap bitmap (bad magic)")
	}
	if v := binary.LittleEndian.Uint32(header[bitmapOffVersion:]); v != bitmapVersion {
		return 0, 0, fmt.Errorf("unsupported version %d", v)
	}
	size, chunkSize = int64(binary.LittleEndian.Uint64(header[bitmapOffSize:])), int64(binary.LittleEndian.Uint64(header[bitmapOffChunk:]))
	if chunkSize <= 0 || chunkSize&(chunkSize-1) != 0 || size <= 0 {
		return 0, 0, fmt.Errorf("corrupt header: size %d, chunk size %d", size, chunkSize)
	}
	return size, chunkSize, nil
}

// preallocate reserves n bytes for f; filesystems without fallocate (tmpfs) are left sparse.
func preallocate(f *os.File, n int64) error {
	if err := unix.Fallocate(int(f.Fd()), 0, 0, n); err != nil && !errors.Is(err, unix.EOPNOTSUPP) {
		return err
	}
	return nil
}
