package util

import (
	"context"
	"net"
	"os"
	"strconv"
	"time"
)

const (
	// NotifyReady, NotifyReloading, NotifyStopping and NotifyWatchdog are the sd_notify(3)
	// states blkmap sends.
	NotifyReady     = "READY=1"
	NotifyReloading = "RELOADING=1"
	NotifyStopping  = "STOPPING=1"
	NotifyWatchdog  = "WATCHDOG=1"
	// notifySocketEnv is set by systemd for Type=notify units.
	notifySocketEnv = "NOTIFY_SOCKET"
	// watchdogUsecEnv and watchdogPIDEnv are set by systemd for units with WatchdogSec=.
	watchdogUsecEnv = "WATCHDOG_USEC"
	watchdogPIDEnv  = "WATCHDOG_PID"
)

// Watchdog feeds systemd's service watchdog (WatchdogSec=) at half its interval for as long
// as healthy says so, until ctx ends; it returns at once, false, when the unit has no
// watchdog (WATCHDOG_USEC unset, or set for another pid). A server whose request path is
// wedged stops being healthy, the pings stop, and systemd kills the process once the
// interval has passed: the kernel fails or requeues the requests of a dead server, never
// those of a stuck one. A healthy server that is merely slow to stop keeps feeding the
// watchdog, so run it past Close.
func Watchdog(ctx context.Context, healthy func() bool) bool {
	interval, ok := watchdogInterval()
	if !ok {
		return false
	}
	ticker := time.NewTicker(interval / 2)
	defer ticker.Stop()
	for {
		if healthy() {
			SdNotify(NotifyWatchdog)
		}
		select {
		case <-ctx.Done():
			return true
		case <-ticker.C:
		}
	}
}

// watchdogInterval reads the watchdog interval systemd set for this process.
func watchdogInterval() (time.Duration, bool) {
	usec, err := strconv.ParseInt(os.Getenv(watchdogUsecEnv), 10, 64)
	if err != nil || usec <= 0 {
		return 0, false
	}
	if pid := os.Getenv(watchdogPIDEnv); pid != "" && pid != strconv.Itoa(os.Getpid()) {
		return 0, false
	}
	return time.Duration(usec) * time.Microsecond, true
}

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
