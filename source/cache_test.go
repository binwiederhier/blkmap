package source

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tier is a mem source that can report ErrNotFound or fail for some offsets.
type tier struct {
	mem
	notFound func(off int64) bool
	fail     func(off int64) bool
	reads    int
	bare     bool // return ErrNotFound unwrapped (no allocation in the fixture)
}

func (t *tier) ReadAt(p []byte, off int64) (int, error) {
	t.reads++
	if t.notFound != nil && t.notFound(off) {
		if t.bare {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("tier: %w", ErrNotFound)
	}
	if t.fail != nil && t.fail(off) {
		return 0, errors.New("tier: connection reset")
	}
	return t.mem.ReadAt(p, off)
}

func TestCache(t *testing.T) {
	t.Parallel()
	fastData, slowData := pattern(4096), pattern(4096)
	for i := range fastData {
		fastData[i] ^= 0xff // distinguishable from slow
	}
	fast := &tier{mem: mem{data: fastData}, notFound: func(off int64) bool { return off >= 1024 && off < 2048 }, fail: func(off int64) bool { return off >= 2048 && off < 3072 }}
	slow := &tier{mem: mem{data: slowData}}
	c := NewCache(fast, slow)
	assert.Equal(t, int64(4096), c.Size())
	p := make([]byte, 512)
	// Hit
	_, err := c.ReadAt(p, 0)
	require.NoError(t, err)
	assert.Equal(t, fastData[:512], p)
	// Miss and failure both fall through to slow
	_, err = c.ReadAt(p, 1024)
	require.NoError(t, err)
	assert.Equal(t, slowData[1024:1536], p)
	_, err = c.ReadAt(p, 2048)
	require.NoError(t, err)
	assert.Equal(t, slowData[2048:2560], p)
	assert.Equal(t, CacheStats{Hits: 1, Misses: 1, Failures: 1}, c.Stats())
	assert.Equal(t, 2, slow.reads)
	// Direct reads skip the fast tier entirely
	_, err = ReadDirect(c, p, 0)
	require.NoError(t, err)
	assert.Equal(t, slowData[:512], p)
	assert.Equal(t, 3, slow.reads)
	assert.Equal(t, CacheStats{Hits: 1, Misses: 1, Failures: 1}, c.Stats())
	// Slow failing too surfaces the slow error
	slow.fail = func(int64) bool { return true }
	_, err = c.ReadAt(p, 1024)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection reset")
	require.NoError(t, c.Close())
	assert.True(t, fast.closed)
	assert.True(t, slow.closed)
}

func TestCacheSizeFollowsSlow(t *testing.T) {
	t.Parallel()
	fast := &tier{mem: mem{data: pattern(1024)}}
	slow := &tier{mem: mem{data: pattern(4096)}}
	c := NewCache(fast, slow)
	assert.Equal(t, int64(4096), c.Size())
	p := make([]byte, 512)
	// Beyond the fast tier's end: a short read there is a miss, not data
	_, err := c.ReadAt(p, 2048)
	require.NoError(t, err)
	assert.Equal(t, pattern(4096)[2048:2560], p)
	// Straddling the fast tier's end
	_, err = c.ReadAt(p, 768)
	require.NoError(t, err)
	assert.Equal(t, pattern(4096)[768:1280], p)
}

func TestCacheHolesAndNoAlloc(t *testing.T) {
	fast := &tier{mem: mem{data: pattern(4096)}, notFound: func(off int64) bool { return off >= 2048 }, bare: true}
	slow := &tier{mem: mem{data: pattern(4096)}}
	c := NewCache(fast, slow)
	holes, err := Holes(c, 0, 4096)
	require.NoError(t, err)
	assert.Nil(t, holes) // mem is not sparse
	p := make([]byte, 512)
	assert.Zero(t, testing.AllocsPerRun(100, func() { c.ReadAt(p, 0) }))    // hit
	assert.Zero(t, testing.AllocsPerRun(100, func() { c.ReadAt(p, 3000) })) // miss, served by slow
}

func BenchmarkCacheReadAt(b *testing.B) {
	fast := &tier{mem: mem{data: pattern(1 << 20)}, notFound: func(off int64) bool { return off >= 512<<10 }, bare: true}
	slow := &tier{mem: mem{data: pattern(1 << 20)}}
	c := NewCache(fast, slow)
	p := make([]byte, 4096)
	for name, off := range map[string]int64{"hit": 0, "miss": 600 << 10} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				c.ReadAt(p, off)
			}
		})
	}
}

func TestCacheShortReadFromSlow(t *testing.T) {
	t.Parallel()
	fast := &tier{mem: mem{data: pattern(100)}, notFound: func(int64) bool { return true }}
	slow := &short{mem: mem{data: pattern(4096)}}
	c := NewCache(fast, slow)
	p := make([]byte, 1000)
	n, err := c.ReadAt(p, 0)
	// The slow tier returned fewer bytes than asked without an error; the rest of p is
	// stale and must not be handed up as data
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.Equal(t, 10, n)
}

// short is a source that returns only 10 bytes of any read, with no error.
type short struct {
	mem
}

func (s *short) ReadAt(p []byte, off int64) (int, error) {
	return s.mem.ReadAt(p[:min(10, len(p))], off)
}

// TestCachePartialCopyAsFastTier: a sparse local copy of the image is a fast tier without a
// map; what it does not hold (its holes) is a miss, never zeros.
func TestCachePartialCopyAsFastTier(t *testing.T) {
	dir := t.TempDir()
	data := pattern(8 << 20)
	slow := &tier{mem: mem{data: data}}
	// The partial copy holds [0, 1M) and [4M, 5M); the rest is holes, which read as zeros
	partial := filepath.Join(dir, "partial.img")
	require.NoError(t, os.WriteFile(partial, data[:1<<20], 0600))
	f, err := os.OpenFile(partial, os.O_RDWR, 0)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(8<<20))
	_, err = f.WriteAt(data[4<<20:5<<20], 4<<20)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	fast, err := OpenFile(partial, 0, 0)
	require.NoError(t, err)
	c := NewCache(fast, slow)
	defer c.Close()
	// Everything reads as the image, holes included
	got := make([]byte, 8<<20)
	_, err = c.ReadAt(got, 0)
	require.NoError(t, err)
	assert.Equal(t, data, got)
	st := c.Stats()
	assert.Positive(t, st.Misses, "ranges the copy lacks are misses")
	assert.Equal(t, int64(0), st.Failures)
	// A read inside the copy is a hit, inside a hole a miss, straddling both a miss
	before := c.Stats()
	p := make([]byte, 4096)
	_, err = c.ReadAt(p, 512<<10)
	require.NoError(t, err)
	assert.Equal(t, before.Hits+1, c.Stats().Hits)
	_, err = c.ReadAt(p, 2<<20)
	require.NoError(t, err)
	assert.Equal(t, before.Misses+1, c.Stats().Misses)
	assert.Equal(t, data[2<<20:2<<20+4096], p)
	_, err = c.ReadAt(p, (1<<20)-2048)
	require.NoError(t, err)
	assert.Equal(t, before.Misses+2, c.Stats().Misses)
	assert.Zero(t, testing.AllocsPerRun(100, func() { c.ReadAt(p, 512<<10) }), "hit")
	assert.Zero(t, testing.AllocsPerRun(100, func() { c.ReadAt(p, 2<<20) }), "miss")
}

func TestCacheMappedFastTier(t *testing.T) {
	t.Parallel()
	data := pattern(4 << 20)
	// A dense copy with stale bytes outside its map: only the mapped part is trusted
	copyData := append([]byte(nil), data...)
	for i := 1 << 20; i < 2<<20; i++ {
		copyData[i] = 0xee
	}
	m, err := NewMap([]Range{{Offset: 0, Length: 1 << 20}, {Offset: 2 << 20, Length: 2 << 20}})
	require.NoError(t, err)
	c := NewCache(WithMap(&mem{data: copyData}, m, 0), &tier{mem: mem{data: data}})
	got := make([]byte, 4<<20)
	_, err = c.ReadAt(got, 0)
	require.NoError(t, err)
	assert.Equal(t, data, got, "the unmapped part comes from the slow tier")
	assert.Equal(t, int64(1), c.Stats().Misses)
}
