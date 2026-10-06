package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidate(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "img")
	require.NoError(t, os.WriteFile(img, make([]byte, 8192), 0600))
	path := filepath.Join(dir, "disk1.yml")
	require.NoError(t, os.WriteFile(path, []byte(`
size: 32K
segments:
  - type: zero
    size: 4K
  - type: file
    path: `+img+`
    source-offset: 1K
  - type: file
    offset: 16K
    path: `+img+`
    size: 4K
`), 0600))
	app, stdout, _ := newTestApp()
	require.NoError(t, app.Run([]string{"blkmap", "validate", path}))
	out := stdout.String()
	assert.Contains(t, out, "disk1")
	assert.Contains(t, out, "32K")
	assert.Contains(t, out, "/dev/blkmap/disk1")
	assert.Contains(t, out, "/var/lib/blkmap/disk1.cow")
	assert.Regexp(t, `0\s+4K\s+zero`, out)
	assert.Regexp(t, `4K\s+7K\s+file\s+`+img+`\s+\(offset 1K\)`, out)
	assert.Regexp(t, `11K\s+5K\s+gap`, out)
	assert.Regexp(t, `16K\s+4K\s+file\s+`+img, out)
	assert.Regexp(t, `20K\s+12K\s+gap`, out)
}

func TestValidateMissingSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d.yml")
	require.NoError(t, os.WriteFile(path, []byte("segments:\n  - type: file\n    path: "+dir+"/missing\n"), 0600))
	app, _, _ := newTestApp()
	err := app.Run([]string{"blkmap", "validate", path})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "segment 0")
	assert.Contains(t, err.Error(), "missing")
}

func TestValidateBadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.yml")
	require.NoError(t, os.WriteFile(path, []byte("segments: []\n"), 0600))
	app, _, _ := newTestApp()
	err := app.Run([]string{"blkmap", "validate", path})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one segment")
}

func TestValidateNoArgs(t *testing.T) {
	app, _, _ := newTestApp()
	err := app.Run([]string{"blkmap", "validate"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ID or FILE")
}

func TestValidateRAID5(t *testing.T) {
	dir := t.TempDir()
	for _, m := range []string{"m0", "m1"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, m), make([]byte, 64<<10), 0600))
	}
	path := filepath.Join(dir, "r.yml")
	require.NoError(t, os.WriteFile(path, []byte(`
segments:
  - type: raid5
    stripe-size: 4K
    members:
      - type: file
        path: `+dir+`/m0
      - type: file
        path: `+dir+`/m1
        source-offset: 4K
      - missing: true
`), 0600))
	app, stdout, _ := newTestApp()
	require.NoError(t, app.Run([]string{"blkmap", "validate", path}))
	out := stdout.String()
	assert.Regexp(t, `0\s+120K\s+raid5 \(3 members, 4K stripes, left-symmetric\)`, out)
	assert.Regexp(t, `member 0\s+file `+dir+`/m0\n`, out)
	assert.Regexp(t, `member 1\s+file `+dir+`/m1 \(offset 4K\)\n`, out)
	assert.Regexp(t, `member 2\s+missing \(reconstructed from parity\)\n`, out)
}

func TestValidateCacheAndHydrate(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"fast", "slow"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, f), make([]byte, 8192), 0600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "list"), []byte("0 4K\n"), 0600))
	path := filepath.Join(dir, "c.yml")
	require.NoError(t, os.WriteFile(path, []byte(`
segments:
  - type: cache
    fast: {type: file, path: `+dir+`/fast, source-offset: 1K}
    slow: {type: file, path: `+dir+`/slow}
hydrate:
  prefetch-list: `+dir+`/list
  rate: 20M
  use-cache: never
`), 0600))
	app, stdout, _ := newTestApp()
	require.NoError(t, app.Run([]string{"blkmap", "validate", path}))
	out := stdout.String()
	assert.Regexp(t, `0\s+8K\s+cache\n`, out)
	assert.Regexp(t, `fast\s+file `+dir+`/fast \(offset 1K\)\n`, out)
	assert.Regexp(t, `slow\s+file `+dir+`/slow\n`, out)
	assert.Contains(t, out, "Hydrate: 1 prefetch ranges from "+dir+"/list, then the rest at 20M/s, cache never")
}

func TestValidateShowsMap(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "img")
	require.NoError(t, os.WriteFile(img, make([]byte, 1<<20), 0600))
	require.NoError(t, os.WriteFile(img+".map", []byte("0 64K\n512K 128K\n"), 0600))
	path := filepath.Join(dir, "m.yml")
	require.NoError(t, os.WriteFile(path, []byte("segments:\n  - type: file\n    path: "+img+"\n    map: "+img+".map\n"), 0600))
	app, stdout, _ := newTestApp()
	require.NoError(t, app.Run([]string{"blkmap", "validate", path}))
	assert.Regexp(t, `file `+img+` \(map: 2 extents, 192K data of 1M\)`, stdout.String())
}

func TestValidateReadOnlyWithoutCOW(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ro.yml")
	require.NoError(t, os.WriteFile(path, []byte("read-only: true\nsegments:\n  - type: zero\n    size: 1M\n"), 0600))
	app, stdout, _ := newTestApp()
	require.NoError(t, app.Run([]string{"blkmap", "validate", path}))
	assert.Contains(t, stdout.String(), "COW:     none (read-only, no hydration)")
	assert.NotContains(t, stdout.String(), "ro.cow")
}
