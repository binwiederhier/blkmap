package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUdevName(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "disk1"), []byte("7\n"), 0600))
	app, stdout, _ := newTestApp()
	require.NoError(t, app.Run([]string{"blkmap", "udev-name", "--run-dir", dir, "ublkb7"}))
	assert.Equal(t, "disk1\n", stdout.String())
	app, _, _ = newTestApp()
	require.Error(t, app.Run([]string{"blkmap", "udev-name", "--run-dir", dir, "ublkb8"}))
}
