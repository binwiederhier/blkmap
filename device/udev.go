package device

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"heckel.io/blkmap/config"
)

const (
	// udevKernelPrefix is the kernel name of a ublk block device; partitions (ublkbNpM)
	// get no blkmap name.
	udevKernelPrefix = "ublkb"
	udevChange       = "change"
)

// UdevName returns the blkmap device id served as kernel device kernelName (ublkbN), from
// the state files in runDir. The udev rule calls it (blkmap udev-name) so that udev, not
// only blkmap, owns /dev/blkmap/<id>: systemd only creates .device units, which fstab
// mounts wait for, for links udev knows about.
func UdevName(runDir, kernelName string) (string, error) {
	digits, ok := strings.CutPrefix(kernelName, udevKernelPrefix)
	id, err := strconv.ParseUint(digits, 10, 32)
	if !ok || err != nil {
		return "", fmt.Errorf("%q is not a ublk disk", kernelName)
	}
	entries, err := os.ReadDir(runDir)
	if err != nil {
		return "", err
	}
	want := strconv.FormatUint(id, 10)
	for _, e := range entries {
		if !e.Type().IsRegular() || !config.ValidID(e.Name()) {
			continue
		}
		content, err := os.ReadFile(filepath.Join(runDir, e.Name()))
		if err == nil && strings.TrimSpace(string(content)) == want {
			return e.Name(), nil
		}
	}
	return "", errors.New("no blkmap device is served as " + kernelName)
}

// announce asks udev to re-run its rules for the block device now that the state file
// names it; the add event came before the name was known.
func announce(blockPath string) error {
	return os.WriteFile(filepath.Join("/sys/block", filepath.Base(blockPath), "uevent"), []byte(udevChange), 0)
}
