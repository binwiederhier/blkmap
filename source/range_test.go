package source

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePrefetch(t *testing.T) {
	t.Parallel()
	ranges, err := ParsePrefetch(strings.NewReader(`
# boot sector and MFT first
0 1M
3G 256M   # trailing comment

1M 1G
`))
	require.NoError(t, err)
	assert.Equal(t, []Range{{0, 1 << 20}, {3 << 30, 256 << 20}, {1 << 20, 1 << 30}}, ranges)
	for _, bad := range []string{"1M", "1M 2M 3M", "x 1M", "1M y", "1M 0", "-1 1M"} {
		_, err := ParsePrefetch(strings.NewReader(bad))
		require.Error(t, err, bad)
		assert.Contains(t, err.Error(), "line 1")
	}
	ranges, err = ParsePrefetch(strings.NewReader(""))
	require.NoError(t, err)
	assert.Empty(t, ranges)
}

func TestParsePrefetchFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "p")
	require.NoError(t, os.WriteFile(path, []byte("0 4K\n"), 0600))
	ranges, err := ParsePrefetchFile(path)
	require.NoError(t, err)
	assert.Equal(t, []Range{{0, 4096}}, ranges)
	_, err = ParsePrefetchFile(filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
}

func TestParseRangesLimit(t *testing.T) {
	t.Parallel()
	_, err := parseRanges(strings.NewReader(strings.Repeat("0 1\n", maxRanges+1)), "list")
	assert.Error(t, err, "more ranges than a sane list holds")
	ranges, err := parseRanges(strings.NewReader(strings.Repeat("0 1\n", 10)), "list")
	require.NoError(t, err)
	assert.Len(t, ranges, 10)
}

func TestParsePrefetchAcceptsRecordings(t *testing.T) {
	t.Parallel()
	// A raw recording works as a prefetch list: reads in order, writes skipped
	ranges, err := ParsePrefetch(strings.NewReader(`# blkmap recording
0 R 0 4096
3 W 8192 4096
41 R 1M 64K
1G 1M
`))
	require.NoError(t, err)
	assert.Equal(t, []Range{{0, 4096}, {1 << 20, 64 << 10}, {1 << 30, 1 << 20}}, ranges)
	for _, bad := range []string{"0 X 0 4K", "-1 R 0 4K", "x R 0 4K", "0 R 0 0", "0 R 0 4K extra"} {
		_, err := ParsePrefetch(strings.NewReader(bad))
		require.Error(t, err, bad)
		assert.Contains(t, err.Error(), "line 1")
	}
}

func TestParseRecording(t *testing.T) {
	t.Parallel()
	accesses, err := ParseRecording(strings.NewReader("# header\n0 R 0 4096\n3 W 8192 4096\n# stopped\n"))
	require.NoError(t, err)
	assert.Equal(t, []Access{{Millis: 0, Offset: 0, Length: 4096}, {Millis: 3, Write: true, Offset: 8192, Length: 4096}}, accesses)
	_, err = ParseRecording(strings.NewReader("0 4096\n"))
	assert.Error(t, err, "a recording line has four fields")
}

func TestCompactRecording(t *testing.T) {
	t.Parallel()
	const chunk = 64 << 10
	compact := CompactRecording([]Access{
		{Millis: 0, Offset: 100, Length: 10},       // chunk 0
		{Millis: 1, Offset: chunk + 5, Length: 10}, // chunk 1: extends the run
		{Millis: 2, Write: true, Offset: 50 * chunk, Length: 4096},
		{Millis: 5, Offset: 0, Length: 4096},               // chunk 0 again: dropped
		{Millis: 7, Offset: 10 * chunk, Length: 2 * chunk}, // chunks 10, 11
		{Millis: 9, Offset: 2*chunk - 1, Length: 2},        // chunks 1 (seen) and 2: new run
		{Millis: 12, Offset: 12 * chunk, Length: 1},        // chunk 12, after chunk 2: new run
	}, chunk)
	assert.Equal(t, []Access{
		{Millis: 0, Offset: 0, Length: 2 * chunk},
		{Millis: 7, Offset: 10 * chunk, Length: 2 * chunk},
		{Millis: 9, Offset: 2 * chunk, Length: chunk},
		{Millis: 12, Offset: 12 * chunk, Length: chunk},
	}, compact)
}

func TestAppendAccess(t *testing.T) {
	b := AppendAccess(nil, Access{Millis: 12, Offset: 4096, Length: 65536})
	b = AppendAccess(b, Access{Millis: 13, Write: true, Offset: 0, Length: 512})
	assert.Equal(t, "12 R 4096 65536\n13 W 0 512\n", string(b))
	buf := make([]byte, 0, 64)
	assert.Zero(t, testing.AllocsPerRun(100, func() { AppendAccess(buf[:0], Access{Millis: 1, Offset: 2, Length: 3}) }))
}
