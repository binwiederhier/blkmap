package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sparse")
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(1<<20))
	_, err = f.WriteAt(make([]byte, 64<<10), 512<<10)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	app, stdout, _ := newTestApp()
	require.NoError(t, app.Run([]string{"blkmap", "map", path}))
	out := stdout.String()
	assert.Contains(t, out, "# data extents of "+path)
	assert.Contains(t, out, "1M total")
	assert.Regexp(t, `(?m)^512K 64K$`, out)
	app, _, _ = newTestApp()
	require.Error(t, app.Run([]string{"blkmap", "map"}))
	require.Error(t, app.Run([]string{"blkmap", "map", filepath.Join(t.TempDir(), "missing")}))
}
