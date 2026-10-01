package cow

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"os"
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
	// bitmapWordBits is the width of the atomic word the bit area is viewed as.
	bitmapWordBits = 32
	bitmapFileMode = 0600
)

var (
	errBitmap = errors.New("bitmap")
)

// Bitmap is a persistent, mmap'd bit-per-chunk map recording which chunks live in the COW
// file. A small header pins the geometry so a stale or foreign bitmap is rejected.
type Bitmap struct {
	f      *os.File
	mapped []byte   // whole file mapping, header included
	words  []uint32 // the bit area, as atomically updatable words
	chunks int64
}

// OpenBitmap opens or creates the bitmap for a device of size bytes and the given chunk size.
func OpenBitmap(path string, size, chunkSize int64) (*Bitmap, error) {
	chunks := (size + chunkSize - 1) / chunkSize
	words := (chunks + bitmapWordBits - 1) / bitmapWordBits
	fileSize := bitmapHeaderSize + words*bitmapWordBits/8
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, bitmapFileMode)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.Size() == 0 {
		err = writeHeader(f, fileSize, size, chunkSize)
	} else {
		err = checkHeader(f, size, chunkSize)
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%w %s: %w", errBitmap, path, err)
	}
	mapped, err := unix.Mmap(int(f.Fd()), 0, int(fileSize), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%w %s: mmap: %w", errBitmap, path, err)
	}
	area := mapped[bitmapHeaderSize:]
	return &Bitmap{
		f:      f,
		mapped: mapped,
		words:  unsafe.Slice((*uint32)(unsafe.Pointer(unsafe.SliceData(area))), words),
		chunks: chunks,
	}, nil
}

// Test reports whether chunk i is marked written.
func (b *Bitmap) Test(i int64) bool {
	return atomic.LoadUint32(&b.words[i/bitmapWordBits])&(1<<(i%bitmapWordBits)) != 0
}

// Set marks chunk i as written.
func (b *Bitmap) Set(i int64) {
	atomic.OrUint32(&b.words[i/bitmapWordBits], 1<<(i%bitmapWordBits))
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

// Sync flushes the mapping to disk.
func (b *Bitmap) Sync() error {
	return unix.Msync(b.mapped, unix.MS_SYNC)
}

// Close syncs and unmaps the bitmap.
func (b *Bitmap) Close() error {
	return errors.Join(b.Sync(), unix.Munmap(b.mapped), b.f.Close())
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
	return f.Truncate(fileSize)
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

// parseHeader validates the magic and version and returns the recorded geometry.
func parseHeader(header []byte) (size, chunkSize int64, err error) {
	if len(header) < bitmapHeaderSize || string(header[:len(bitmapMagic)]) != bitmapMagic {
		return 0, 0, fmt.Errorf("not a blkmap bitmap (bad magic)")
	}
	if v := binary.LittleEndian.Uint32(header[bitmapOffVersion:]); v != bitmapVersion {
		return 0, 0, fmt.Errorf("unsupported version %d", v)
	}
	return int64(binary.LittleEndian.Uint64(header[bitmapOffSize:])), int64(binary.LittleEndian.Uint64(header[bitmapOffChunk:])), nil
}
