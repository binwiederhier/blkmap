package util

import (
	"net"
	"os"
)

const (
	// NotifyReady and NotifyStopping are the sd_notify(3) states blkmap sends.
	NotifyReady    = "READY=1"
	NotifyStopping = "STOPPING=1"
	// notifySocketEnv is set by systemd for Type=notify units.
	notifySocketEnv = "NOTIFY_SOCKET"
)

// SdNotify sends a state string to systemd's notify socket. It is a no-op (returning false)
// when not running under a Type=notify unit.
func SdNotify(state string) (bool, error) {
	path := os.Getenv(notifySocketEnv)
	if path == "" {
		return false, nil
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(state)); err != nil {
		return false, err
	}
	return true, nil
}
