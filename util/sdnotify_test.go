package util

import (
	"context"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSdNotify(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notify.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	t.Setenv(notifySocketEnv, path)
	sent, err := SdNotify(NotifyReady)
	require.NoError(t, err)
	assert.True(t, sent)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, NotifyReady, string(buf[:n]))
}

func TestWatchdogFeedsWhileHealthy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notify.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	t.Setenv(notifySocketEnv, path)
	t.Setenv(watchdogUsecEnv, "100000") // pings every 50 ms
	t.Setenv(watchdogPIDEnv, "")
	var healthy atomic.Bool
	healthy.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- Watchdog(ctx, healthy.Load) }()
	buf := make([]byte, 64)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	n, err := conn.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, NotifyWatchdog, string(buf[:n]))
	healthy.Store(false)
	time.Sleep(150 * time.Millisecond) // pings already in flight land
	drain := make([]byte, 64)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(20*time.Millisecond)))
	for {
		if _, err := conn.Read(drain); err != nil {
			break
		}
	}
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(300*time.Millisecond)))
	_, err = conn.Read(buf)
	assert.Error(t, err, "an unhealthy process must not feed the watchdog")
	cancel()
	assert.True(t, <-done)
}

func TestWatchdogWithoutWatchdogSec(t *testing.T) {
	t.Setenv(watchdogUsecEnv, "")
	assert.False(t, Watchdog(context.Background(), func() bool { return true }))
	t.Setenv(watchdogUsecEnv, "100000")
	t.Setenv(watchdogPIDEnv, "1")
	assert.False(t, Watchdog(context.Background(), func() bool { return true }), "a watchdog armed for another pid is not ours")
}

func TestSdNotifyNoSocket(t *testing.T) {
	t.Setenv(notifySocketEnv, "")
	sent, err := SdNotify(NotifyReady)
	require.NoError(t, err)
	assert.False(t, sent)
}
