package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"heckel.io/blkmap/config"
	"heckel.io/blkmap/cow"
	"heckel.io/blkmap/source"
)

func TestLogDiskContentAndHoles(t *testing.T) {
	d, err := newLogDisk(64<<20, map[string]string{"name": "t", "version": "1"})
	require.NoError(t, err)
	p := make([]byte, 64)
	_, err = d.ReadAt(p, 0)
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(p, []byte("block 0 of t\n")))
	_, err = d.ReadAt(p, 2<<20) // inside the hole of the first stripe
	require.NoError(t, err)
	assert.Equal(t, make([]byte, 64), p)
	holes, err := source.Holes(d, 0, 32<<20)
	require.NoError(t, err)
	assert.Equal(t, []source.Range{{Offset: 1 << 20, Length: 15 << 20}, {Offset: 17 << 20, Length: 15 << 20}}, holes)
}

// TestConfigWithCustomSegment builds the example's config path without a kernel device: the
// registered constructor is found by name, and the COW store pins the source's identity.
func TestConfigWithCustomSegment(t *testing.T) {
	source.Register("logdisk", newLogDisk)
	data, err := os.ReadFile("logdisk.yml")
	require.NoError(t, err)
	c, err := config.Parse("logdisk", data)
	require.NoError(t, err)
	base, err := source.FromConfig(c)
	require.NoError(t, err)
	assert.Equal(t, int64(320<<20), base.Size())
	dir := t.TempDir()
	s, err := cow.OpenWith(base, &cow.Options{COWFile: filepath.Join(dir, "c.cow"), Bitmap: filepath.Join(dir, "c.bitmap"), ChunkSize: 64 << 10, Identity: source.Identity(base)})
	require.NoError(t, err)
	_, err = s.WriteAt([]byte("hello"), 0)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	// The same source with another version is different content: the overlay is refused
	changed, err := newLogDisk(256<<20, map[string]string{"name": "logdisk", "version": "2"})
	require.NoError(t, err)
	other, err := source.NewConcat([]*source.Segment{{Offset: 0, Source: changed}, {Offset: 256 << 20, Source: source.NewZero(64 << 20)}}, 0)
	require.NoError(t, err)
	_, err = cow.OpenWith(other, &cow.Options{COWFile: filepath.Join(dir, "c.cow"), Bitmap: filepath.Join(dir, "c.bitmap"), ChunkSize: 64 << 10, Identity: source.Identity(other)})
	assert.ErrorContains(t, err, "source changed")
}
