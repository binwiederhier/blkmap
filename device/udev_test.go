package device

import (
	"os"
	"path/filepath"
	"testing"

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
