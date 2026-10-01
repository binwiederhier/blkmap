package source

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"heckel.io/blkmap/config"
)

func TestFromConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	img := filepath.Join(dir, "img")
	require.NoError(t, os.WriteFile(img, pattern(8192), 0600))
	c, err := config.Parse("d", []byte(`
size: 20K
segments:
  - type: zero
    size: 1K
  - type: file
    path: `+img+`
    source-offset: 512
  - type: file
    offset: 16K
    path: `+img+`
    size: 1K
`))
	require.NoError(t, err)
	src, err := FromConfig(c)
	require.NoError(t, err)
	t.Cleanup(func() { src.Close() })
	assert.Equal(t, int64(20<<10), src.Size())
	segs := src.Segments()
	require.Len(t, segs, 3)
	assert.Equal(t, int64(0), segs[0].Offset)
	assert.Equal(t, int64(1024), segs[1].Offset)
	assert.Equal(t, int64(8192-512), segs[1].Source.Size())
	assert.Equal(t, int64(16<<10), segs[2].Offset)
	assert.Equal(t, int64(1024), segs[2].Source.Size())
	p := make([]byte, 20<<10)
	_, err = src.ReadAt(p, 0)
	require.NoError(t, err)
	assert.Equal(t, make([]byte, 1024), p[:1024])
	assert.Equal(t, pattern(8192)[512:], p[1024:1024+8192-512])
	assert.Equal(t, pattern(8192)[:1024], p[16<<10:17<<10])
	assert.Equal(t, make([]byte, 3<<10), p[17<<10:])
}

func TestFromConfigMissingFile(t *testing.T) {
	t.Parallel()
	c, err := config.Parse("d", []byte("segments:\n  - type: file\n    path: /nonexistent/x\n"))
	require.NoError(t, err)
	_, err = FromConfig(c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "segment 0")
}

func TestFromConfigRAID5(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A 3-member left-symmetric array of 2 rows with 1 KiB stripes, member 2 missing; the
	// member files carry 512 bytes of junk before the array data to exercise source-offset
	image := pattern(2 * 1024 * 2)
	members := buildArray(t, image, 3, LeftSymmetric)
	paths := make([]string, 3)
	for i, m := range members {
		paths[i] = filepath.Join(dir, fmt.Sprintf("m%d", i))
		require.NoError(t, os.WriteFile(paths[i], append(make([]byte, 512), m.(*mem).data...), 0600))
	}
	c, err := config.Parse("r", []byte(`
segments:
  - type: raid5
    stripe-size: 1K
    members:
      - type: file
        path: `+paths[0]+`
        source-offset: 512
      - type: file
        path: `+paths[1]+`
        source-offset: 512
      - missing: true
`))
	require.NoError(t, err)
	src, err := FromConfig(c)
	require.NoError(t, err)
	t.Cleanup(func() { src.Close() })
	assert.Equal(t, int64(len(image)), src.Size())
	got := make([]byte, len(image))
	_, err = src.ReadAt(got, 0)
	require.NoError(t, err)
	assert.Equal(t, image, got)
}

func TestFromConfigRAID5MissingMemberFile(t *testing.T) {
	t.Parallel()
	c, err := config.Parse("r", []byte("segments:\n  - type: raid5\n    members:\n      - type: file\n        path: /nonexistent/a\n      - type: file\n        path: /nonexistent/b\n      - type: file\n        path: /nonexistent/c\n"))
	require.NoError(t, err)
	_, err = FromConfig(c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "segment 0")
	assert.Contains(t, err.Error(), "member 0")
}

func TestFromConfigCache(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fast, slow := filepath.Join(dir, "fast"), filepath.Join(dir, "slow")
	require.NoError(t, os.WriteFile(fast, pattern(4096), 0600))
	require.NoError(t, os.WriteFile(slow, pattern(8192), 0600))
	c, err := config.Parse("c", []byte(`
segments:
  - type: cache
    fast:
      type: file
      path: `+fast+`
    slow:
      type: file
      path: `+slow+`
`))
	require.NoError(t, err)
	src, err := FromConfig(c)
	require.NoError(t, err)
	t.Cleanup(func() { src.Close() })
	assert.Equal(t, int64(8192), src.Size())
	p := make([]byte, 8192)
	_, err = src.ReadAt(p, 0)
	require.NoError(t, err)
	assert.Equal(t, pattern(8192), p)
	// A missing tier file is reported with its place
	c, err = config.Parse("c", []byte("segments:\n  - type: cache\n    fast: {type: file, path: /nonexistent}\n    slow: {type: file, path: "+slow+"}\n"))
	require.NoError(t, err)
	_, err = FromConfig(c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "segment 0: fast:")
}
