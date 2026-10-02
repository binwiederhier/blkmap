package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReap(t *testing.T) {
	app, _, _ := newTestApp()
	require.NoError(t, app.Run([]string{"blkmap", "reap", "--run-dir", t.TempDir(), "nothing"}))
}
