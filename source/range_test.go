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
