package device

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
	for _, e := range entries {
		if !e.Type().IsRegular() || !config.ValidID(e.Name()) {
			continue
		}
		if served, _, ok := readState(filepath.Join(runDir, e.Name())); ok && uint64(served) == id {
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

// writeState records the kernel device id and server pid of a served device. The pid tells
// a successor whether a device under that id is still its predecessor's: the kernel hands
// a freed id to the next device created.
func writeState(path string, id uint32, pid int) error {
	return os.WriteFile(path, []byte(fmt.Sprintf("%d %d\n", id, pid)), stateFileMode)
}

// readState parses a state file ("ID PID", or just "ID" from older versions); ok is false
// if it is missing or malformed.
func readState(path string) (id uint32, pid int, ok bool) {
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	fields := strings.Fields(string(content))
	if len(fields) < 1 || len(fields) > 2 {
		return 0, 0, false
	}
	n, err := strconv.ParseUint(fields[0], 10, 32)
	if err != nil {
		return 0, 0, false
	}
	if len(fields) == 2 {
		if pid, err = strconv.Atoi(fields[1]); err != nil || pid < 0 {
			return 0, 0, false
		}
	}
	return uint32(n), pid, true
}

// ownsKernelID reports whether name's state file is the newest one claiming kernel device
// id, i.e. whether that device is name's. Stale files from servers whose device was deleted
// may name an id the kernel has since given to another device.
func ownsKernelID(runDir, name string, id uint32) bool {
	entries, err := os.ReadDir(runDir)
	if err != nil {
		return false
	}
	newest, owner := time.Time{}, ""
	for _, e := range entries {
		if !e.Type().IsRegular() || !config.ValidID(e.Name()) {
			continue
		}
		claimed, _, ok := readState(filepath.Join(runDir, e.Name()))
		info, err := e.Info()
		if !ok || claimed != id || err != nil {
			continue
		}
		if info.ModTime().After(newest) {
			newest, owner = info.ModTime(), e.Name()
		}
	}
	return owner == name
}
