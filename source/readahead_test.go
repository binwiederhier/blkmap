package source

import (
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadAhead(t *testing.T) {
	t.Parallel()
	inner := &recordingSource{mem: mem{data: pattern(16*cacheBlockSize + 100)}}
	r := NewReadAhead(inner)
	assert.Equal(t, int64(len(inner.data)), r.Size())
	p := make([]byte, 4096)
	// First read: one whole block fetched from the inner source
	_, err := r.ReadAt(p, 100)
	require.NoError(t, err)
	assert.Equal(t, inner.data[100:4196], p)
	assert.Equal(t, []Range{{0, cacheBlockSize}}, inner.recorded())
	// Sequential continuation triggers read-ahead of the next blocks
	_, err = r.ReadAt(p, 4196)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(inner.recorded()) >= 1+readAheadBlocks }, 5*time.Second, 5*time.Millisecond)
	// The partial tail block and EOF semantics
	tail := make([]byte, 200)
	n, err := r.ReadAt(tail, 16*cacheBlockSize)
	assert.Equal(t, 100, n)
	assert.ErrorIs(t, err, io.EOF)
	assert.Equal(t, inner.data[16*cacheBlockSize:], tail[:100])
	// Direct reads bypass the cache
	// Let the read-ahead settle before counting: no new fetch for a while
	before := len(inner.recorded())
	for settled := 0; settled < 10; settled++ {
		time.Sleep(5 * time.Millisecond)
		if n := len(inner.recorded()); n != before {
			before, settled = n, 0
		}
	}
	_, err = ReadDirect(r, p, 100)
	require.NoError(t, err)
	assert.Equal(t, inner.data[100:4196], p)
	reads := inner.recorded()
	assert.Equal(t, before+1, len(reads))
	assert.Equal(t, Range{100, 4096}, reads[before])
	require.NoError(t, r.Close())
	assert.True(t, inner.closed)
}

func TestReadAheadHolesAndMap(t *testing.T) {
	t.Parallel()
	inner := &recordingSource{mem: mem{data: pattern(4 * cacheBlockSize)}}
	m, err := NewMap([]Range{{0, cacheBlockSize}})
	require.NoError(t, err)
	// Map outside, read-ahead inside: holes never reach the cache or the inner source
	src := WithMap(NewReadAhead(inner), m, 0)
	holes, err := Holes(src, 0, 4*cacheBlockSize)
	require.NoError(t, err)
	assert.Equal(t, []Range{{cacheBlockSize, 3 * cacheBlockSize}}, holes)
	p := make([]byte, 4096)
	_, err = src.ReadAt(p, 2*cacheBlockSize)
	require.NoError(t, err)
	assert.Equal(t, make([]byte, 4096), p)
	assert.Empty(t, inner.recorded())
	// Read-ahead itself forwards Holes to a sparse inner source
	z := NewReadAhead(NewZero(1000))
	holes, err = Holes(z, 10, 20)
	require.NoError(t, err)
	assert.Equal(t, []Range{{10, 20}}, holes)
}

func TestReadAheadNoAllocOnHit(t *testing.T) {
	inner := &mem{data: pattern(2 * cacheBlockSize)}
	r := NewReadAhead(inner)
	p := make([]byte, 4096)
	_, err := r.ReadAt(p, 0)
	require.NoError(t, err)
	assert.Zero(t, testing.AllocsPerRun(100, func() { r.ReadAt(p, 100<<10) }))
}
