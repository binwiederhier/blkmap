package device

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUdevName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "disk1"), []byte("3\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "disk2"), []byte("12\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "junk"), []byte("x\n"), 0600))
	name, err := UdevName(dir, "ublkb3")
	require.NoError(t, err)
	assert.Equal(t, "disk1", name)
	name, err = UdevName(dir, "ublkb12")
	require.NoError(t, err)
	assert.Equal(t, "disk2", name)
	// Not ours, a partition, or another driver's device: no name, so udev adds no link
	for _, kernel := range []string{"ublkb1", "ublkb3p1", "sda", "ublkb", ""} {
		_, err = UdevName(dir, kernel)
		assert.Error(t, err, kernel)
	}
	_, err = UdevName(filepath.Join(dir, "missing"), "ublkb3")
	assert.Error(t, err)
}

func TestStateFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "d")
	require.NoError(t, writeState(path, 7, 4242))
	id, pid, ok := readState(path)
	require.True(t, ok)
	assert.Equal(t, uint32(7), id)
	assert.Equal(t, 4242, pid)
	// The format before pids were recorded
	require.NoError(t, os.WriteFile(path, []byte("9\n"), 0600))
	id, pid, ok = readState(path)
	require.True(t, ok)
	assert.Equal(t, uint32(9), id)
	assert.Zero(t, pid)
	for _, bad := range []string{"", "x", "1 2 3", "-1 5"} {
		require.NoError(t, os.WriteFile(path, []byte(bad), 0600))
		_, _, ok = readState(path)
		assert.False(t, ok, bad)
	}
	_, _, ok = readState(filepath.Join(t.TempDir(), "missing"))
	assert.False(t, ok)
	// udev names a device from either format
	dir := filepath.Dir(path)
	require.NoError(t, writeState(path, 11, 1))
	name, err := UdevName(dir, "ublkb11")
	require.NoError(t, err)
	assert.Equal(t, "d", name)
}

func TestReapWithoutState(t *testing.T) {
	t.Parallel()
	assert.NoError(t, Reap("none", t.TempDir()))
}

func TestOwnsKernelID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, writeState(filepath.Join(dir, "old"), 3, 100))
	past := time.Now().Add(-time.Minute)
	require.NoError(t, os.Chtimes(filepath.Join(dir, "old"), past, past))
	require.NoError(t, writeState(filepath.Join(dir, "new"), 3, 200))
	require.NoError(t, writeState(filepath.Join(dir, "other"), 4, 300))
	// The kernel reuses freed ids: the newest claim is the one that holds the device
	assert.True(t, ownsKernelID(dir, "new", 3))
	assert.False(t, ownsKernelID(dir, "old", 3))
	assert.True(t, ownsKernelID(dir, "other", 4))
	assert.False(t, ownsKernelID(dir, "missing", 3))
}
