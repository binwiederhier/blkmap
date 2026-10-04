package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const recording = `# blkmap recording of d
0 R 0 4096
2 R 65536 4096
3 W 8192 4096
5 R 0 4096
1500 R 10485760 1048576
2500 R 1048576 65536
`

func TestRecordingCompact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.rec")
	require.NoError(t, os.WriteFile(path, []byte(recording), 0600))
	var stdout bytes.Buffer
	app := New("test", "", "")
	app.Writer = &stdout
	require.NoError(t, app.Run([]string{"blkmap", "recording", "compact", path}))
	assert.Equal(t, "0 R 0 131072\n1500 R 10485760 1048576\n2500 R 1048576 65536\n", stdout.String())
}

func TestRecordingStats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.rec")
	require.NoError(t, os.WriteFile(path, []byte(recording), 0600))
	var stdout bytes.Buffer
	app := New("test", "", "")
	app.Writer = &stdout
	require.NoError(t, app.Run([]string{"blkmap", "recording", "stats", path}))
	out := stdout.String()
	assert.Contains(t, out, "requests:  6 (5 reads, 1 writes) over 2.5s")
	assert.Contains(t, out, "unique:    1.2M read, in 3 ranges")
	assert.Contains(t, out, "by 1s     128K")
	assert.Contains(t, out, "by 5s     1.2M")
	// 1152K by 1.5 s is the steepest point after the first second: 768K/s keeps ahead
	assert.Contains(t, out, "rate:      768K/s from the start keeps ahead of the reads after the first second")
}

// The old name stays as a hidden alias for one release.
func TestPrefetchAlias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.rec")
	require.NoError(t, os.WriteFile(path, []byte(recording), 0600))
	var stdout bytes.Buffer
	app := New("test", "", "")
	app.Writer = &stdout
	require.NoError(t, app.Run([]string{"blkmap", "prefetch", path}))
	assert.Equal(t, "0 R 0 131072\n1500 R 10485760 1048576\n2500 R 1048576 65536\n", stdout.String())
	stdout.Reset()
	require.NoError(t, app.Run([]string{"blkmap", "prefetch", "--stats", path}))
	assert.Contains(t, stdout.String(), "requests:  6")
	assert.True(t, cmdPrefetch.Hidden, "the alias is hidden from help")
	assert.False(t, cmdRecording.Hidden)
}
