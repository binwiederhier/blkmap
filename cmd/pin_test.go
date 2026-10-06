package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"heckel.io/blkmap/cow"
	"heckel.io/blkmap/source"
)

func TestPin(t *testing.T) {
	dir := t.TempDir()
	img, cowPath := filepath.Join(dir, "img"), filepath.Join(dir, "d.cow")
	require.NoError(t, os.WriteFile(img, make([]byte, 1<<20), 0600))
	conf := filepath.Join(dir, "d.yml")
	require.NoError(t, os.WriteFile(conf, []byte("cow:\n  file: "+cowPath+"\nsegments:\n  - type: file\n    path: "+img+"\n"), 0600))
	// A cow file with writes over the image as it was
	src, err := source.OpenFile(img, 0, 0)
	require.NoError(t, err)
	s, err := cow.OpenWith(src, &cow.Options{COWFile: cowPath, Bitmap: cowPath + ".bitmap", ChunkSize: 65536, Identity: source.Identity(src)})
	require.NoError(t, err)
	_, err = s.WriteAt(make([]byte, 65536), 0)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	// The image is touched (same bytes); the operator accepts it
	later := time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(img, later, later))
	app, stdout, _ := newTestApp()
	require.NoError(t, app.Run([]string{"blkmap", "pin", "--config", conf, "d"}))
	assert.Contains(t, stdout.String(), "pinned")
	src, err = source.OpenFile(img, 0, 0)
	require.NoError(t, err)
	info, err := cow.Inspect(cowPath + ".bitmap")
	require.NoError(t, err)
	assert.Contains(t, info.Identity, source.Identity(src), "the config wraps the file in its layout")
	src.Close()
	// Not while the device is served: the cow file is locked
	src, err = source.OpenFile(img, 0, 0)
	require.NoError(t, err)
	s, err = cow.Open(src, cowPath, cowPath+".bitmap", 65536)
	require.NoError(t, err)
	app, _, _ = newTestApp()
	err = app.Run([]string{"blkmap", "pin", "--config", conf, "d"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "in use")
	require.NoError(t, s.Close())
}

func TestPinReadOnlyWithoutCOW(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "ro.yml")
	require.NoError(t, os.WriteFile(conf, []byte("read-only: true\ncow:\n  file: "+filepath.Join(dir, "ro.cow")+"\nsegments:\n  - type: zero\n    size: 1M\n"), 0600))
	app, stdout, _ := newTestApp()
	require.NoError(t, app.Run([]string{"blkmap", "pin", "--config", conf, "ro"}))
	assert.Contains(t, stdout.String(), "no cow file")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "only the config: pin creates nothing")
}
