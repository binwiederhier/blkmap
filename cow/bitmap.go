package cow

import (
	"encoding/binary"
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
	// Header layout: magic[8] version[4] pad[4] size[8] chunk[8]
	bitmapOffVersion = 8
	bitmapOffSize    = 16
	bitmapOffChunk   = 24
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
type Bitmap struct {
	f      *os.File
	words  []uint32
	chunks int64
	dirty  []atomic.Bool // one per bitmapPageSize of the bit area
	syncMu sync.Mutex    // Serializes Sync
}

// OpenBitmap opens or creates the bitmap for a device of size bytes and the given chunk size.
func OpenBitmap(path string, size, chunkSize int64) (*Bitmap, error) {
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
	return b, nil
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

// Close syncs and closes the bitmap.
func (b *Bitmap) Close() error {
	return errors.Join(b.Sync(), b.f.Close())
}

// CloseNoSync closes the bitmap without writing pending bits, for when their data is not
// known to be durable.
func (b *Bitmap) CloseNoSync() error {
	return b.f.Close()
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
