package source

import (
	"errors"
	"fmt"
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
}

func (t *tier) ReadAt(p []byte, off int64) (int, error) {
	t.reads++
	if t.notFound != nil && t.notFound(off) {
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
