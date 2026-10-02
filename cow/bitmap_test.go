package cow

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestBitmap(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "bits")
	b, err := OpenBitmap(path, 100*4096+1, 4096) // 101 chunks: the partial tail counts
	require.NoError(t, err)
	assert.Equal(t, int64(101), b.Chunks())
	assert.Equal(t, int64(0), b.Count())
	assert.False(t, b.Test(0))
	b.Set(0)
	b.Set(31)
	b.Set(32)
	b.Set(100)
	assert.True(t, b.Test(0))
	assert.True(t, b.Test(31))
	assert.True(t, b.Test(32))
	assert.True(t, b.Test(100))
	assert.False(t, b.Test(1))
	assert.False(t, b.Test(99))
	assert.Equal(t, int64(4), b.Count())
	require.NoError(t, b.Sync())
	require.NoError(t, b.Close())
	// Persisted across reopen
	b, err = OpenBitmap(path, 100*4096+1, 4096)
	require.NoError(t, err)
	assert.Equal(t, int64(4), b.Count())
	assert.True(t, b.Test(100))
	assert.False(t, b.Test(50))
	require.NoError(t, b.Close())
	// Geometry mismatch is rejected
	_, err = OpenBitmap(path, 100*4096+1, 8192)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chunk size")
	_, err = OpenBitmap(path, 200*4096, 4096)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "device size")
	// Garbage is rejected
	require.NoError(t, os.WriteFile(path, []byte("not a bitmap"), 0600))
	_, err = OpenBitmap(path, 100*4096+1, 4096)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a blkmap bitmap")
}

func TestBitmapFileSize(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "bits")
	b, err := OpenBitmap(path, 1<<40, 64<<10) // 1 TiB at 64 KiB chunks = 16M bits = 2 MiB
	require.NoError(t, err)
	require.NoError(t, b.Close())
	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, int64(bitmapHeaderSize+2<<20), st.Size()) // 2 MiB of bits, page aligned
}

func TestBitmapPersistsOnlyOnSync(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "bits")
	b, err := OpenBitmap(path, 1000*4096, 4096)
	require.NoError(t, err)
	b.Set(1)
	b.Set(999)
	// Nothing reaches the file before Sync: a crash here must not leave a bit whose data
	// the COW file may not have
	other, err := OpenBitmap(path, 1000*4096, 4096)
	require.NoError(t, err)
	assert.Equal(t, int64(0), other.Count())
	require.NoError(t, other.Close())
	require.NoError(t, b.Sync())
	other, err = OpenBitmap(path, 1000*4096, 4096)
	require.NoError(t, err)
	assert.Equal(t, int64(2), other.Count())
	assert.True(t, other.Test(999))
	require.NoError(t, other.Close())
	// Bits set after a Sync are picked up by the next one
	b.Set(500)
	require.NoError(t, b.Sync())
	info, err := Inspect(path)
	require.NoError(t, err)
	assert.Equal(t, int64(3), info.Written)
	assert.Equal(t, int64(1000), info.Chunks)
	require.NoError(t, b.Close())
}

func TestBitmapNoAlloc(t *testing.T) {
	b, err := OpenBitmap(filepath.Join(t.TempDir(), "bits"), 1<<30, 64<<10)
	require.NoError(t, err)
	defer b.Close()
	assert.Zero(t, testing.AllocsPerRun(100, func() { b.Set(1000); b.Test(1000) }))
}

func BenchmarkBitmap(b *testing.B) {
	bm, err := OpenBitmap(filepath.Join(b.TempDir(), "bits"), 1<<40, 64<<10)
	if err != nil {
		b.Fatal(err)
	}
	defer bm.Close()
	b.Run("set", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			bm.Set(int64(i) % bm.Chunks())
		}
	})
	b.Run("test", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			bm.Test(int64(i) % bm.Chunks())
		}
	})
	b.Run("count-1TiB", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			bm.Count()
		}
	})
	b.Run("sync-dirty-page", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			bm.Set(int64(i) % bm.Chunks())
			bm.Sync()
		}
	})
}

func TestOpenBitmapPreallocates(t *testing.T) {
	t.Parallel()
	// A full cow filesystem must not stop the bitmap from recording what was written, so
	// the file is allocated up front rather than sparse
	path := filepath.Join(t.TempDir(), "p.bitmap")
	b, err := OpenBitmap(path, 1<<30, 4096)
	require.NoError(t, err)
	require.NoError(t, b.Close())
	var st unix.Stat_t
	require.NoError(t, unix.Stat(path, &st))
	assert.GreaterOrEqual(t, st.Blocks*512, st.Size)
}
