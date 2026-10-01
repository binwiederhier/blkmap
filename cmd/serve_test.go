package cmd

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServeNoArgs(t *testing.T) {
	app, _, _ := newTestApp()
	err := app.Run([]string{"blkmap", "serve"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "device ID")
}

func TestServeBadID(t *testing.T) {
	app, _, _ := newTestApp()
	err := app.Run([]string{"blkmap", "serve", "../etc"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid device id")
}

func TestServeMissingConfig(t *testing.T) {
	app, _, _ := newTestApp()
	err := app.Run([]string{"blkmap", "serve", "--config", filepath.Join(t.TempDir(), "nope.yml"), "d"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nope.yml")
}
