package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseFull(t *testing.T) {
	t.Parallel()
	c, err := Parse("disk1", []byte(`
size: 10G
block-size: 4096
read-only: true
cow:
  file: /tmp/disk1.cow
  bitmap: /tmp/disk1.bits
  chunk-size: 1M
segments:
  - type: zero
    size: 1M
  - type: file
    path: /srv/part1.img
    source-offset: 512
    size: 100M
  - type: device
    offset: 200M
    path: /dev/sdb1
  - type: http
    url: https://example.com/disk.img
    size: 2G
`))
	require.NoError(t, err)
	assert.Equal(t, "disk1", c.ID)
	assert.Equal(t, int64(10<<30), c.Size)
	assert.Equal(t, 4096, c.BlockSize)
	assert.True(t, c.ReadOnly)
	assert.Equal(t, "/tmp/disk1.cow", c.COW.File)
	assert.Equal(t, "/tmp/disk1.bits", c.COW.Bitmap)
	assert.Equal(t, int64(1<<20), c.COW.ChunkSize)
	require.Len(t, c.Segments, 4)
	assert.Equal(t, &Segment{Type: SourceZero, Offset: -1, Size: 1 << 20}, c.Segments[0])
	assert.Equal(t, &Segment{Type: SourceFile, Offset: -1, Size: 100 << 20, Path: "/srv/part1.img", SourceOffset: 512}, c.Segments[1])
	assert.Equal(t, &Segment{Type: SourceDevice, Offset: 200 << 20, Path: "/dev/sdb1"}, c.Segments[2])
	assert.Equal(t, &Segment{Type: SourceHTTP, Offset: -1, Size: 2 << 30, URL: "https://example.com/disk.img"}, c.Segments[3])
}

func TestParseDefaults(t *testing.T) {
	t.Parallel()
	c, err := Parse("d", []byte("segments:\n  - type: zero\n    size: 4K\n"))
	require.NoError(t, err)
	assert.Equal(t, int64(0), c.Size)
	assert.Equal(t, DefaultBlockSize, c.BlockSize)
	assert.False(t, c.ReadOnly)
	assert.Equal(t, DefaultStateDir+"/d.cow", c.COW.File)
	assert.Equal(t, DefaultStateDir+"/d.cow.bitmap", c.COW.Bitmap)
	assert.Equal(t, int64(DefaultChunkSize), c.COW.ChunkSize)
}

func TestParseJSON(t *testing.T) {
	t.Parallel()
	c, err := Parse("d", []byte(`{"size": "1M", "segments": [{"type": "zero", "size": "1M"}]}`))
	require.NoError(t, err)
	assert.Equal(t, int64(1<<20), c.Size)
	require.Len(t, c.Segments, 1)
}

func TestParseErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		errMsg  string
	}{
		{"no segments", "size: 1M\n", "at least one segment"},
		{"unknown type", "segments:\n  - type: nope\n    size: 1M\n", "unknown segment type"},
		{"zero without size", "segments:\n  - type: zero\n", "zero segment needs a size"},
		{"file without path", "segments:\n  - type: file\n", "needs a path"},
		{"device without path", "segments:\n  - type: device\n", "needs a path"},
		{"http without url", "segments:\n  - type: http\n", "needs a url"},
		{"http with path", "segments:\n  - type: http\n    url: http://x/y\n    path: /x\n", "path is only valid"},
		{"file with url", "segments:\n  - type: file\n    path: /x\n    url: http://x/y\n", "url is only valid"},
		{"bad size", "size: 1X\nsegments:\n  - type: zero\n    size: 1M\n", "size"},
		{"bad block size", "block-size: 1024\nsegments:\n  - type: zero\n    size: 1M\n", "block-size must be 512 or 4096"},
		{"chunk not power of two", "cow:\n  chunk-size: 3000\nsegments:\n  - type: zero\n    size: 1M\n", "power of two"},
		{"chunk smaller than block", "block-size: 4096\ncow:\n  chunk-size: 512\nsegments:\n  - type: zero\n    size: 1M\n", "at least the block size"},
		{"segment size not block aligned", "segments:\n  - type: zero\n    size: 1000\n", "multiple of block-size"},
		{"segment offset not block aligned", "segments:\n  - type: zero\n    offset: 100\n    size: 512\n", "multiple of block-size"},
		{"device size not block aligned", "size: 1000\nsegments:\n  - type: zero\n    size: 512\n", "multiple of block-size"},
		{"unknown key", "sizes: 1M\nsegments:\n  - type: zero\n    size: 1M\n", "field sizes not found"},
		{"bad yaml", "segments: [\n", "yaml"},
		{"empty", "", "at least one segment"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse("d", []byte(tt.content))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errMsg)
		})
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := Path(dir, "d")
	assert.Equal(t, filepath.Join(dir, "d.yml"), path)
	require.NoError(t, os.WriteFile(path, []byte("segments:\n  - type: zero\n    size: 1M\n"), 0600))
	c, err := Load("d", path)
	require.NoError(t, err)
	assert.Equal(t, "d", c.ID)
	_, err = Load("d", filepath.Join(dir, "missing.yml"))
	require.Error(t, err)
}

func TestValidID(t *testing.T) {
	t.Parallel()
	assert.True(t, ValidID("disk1"))
	assert.True(t, ValidID("my-disk_2.img"))
	assert.False(t, ValidID(""))
	assert.False(t, ValidID("../etc"))
	assert.False(t, ValidID("a/b"))
	assert.False(t, ValidID(".hidden"))
	assert.False(t, ValidID("with space"))
	assert.False(t, ValidID("-leading-dash"))
}

func TestParseRejectsBadID(t *testing.T) {
	t.Parallel()
	_, err := Parse("../x", []byte("segments:\n  - type: zero\n    size: 1M\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid device id")
}
