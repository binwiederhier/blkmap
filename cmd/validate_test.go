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
