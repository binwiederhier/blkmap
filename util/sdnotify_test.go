package util

import (
	"net"
	"path/filepath"
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

func TestSdNotifyNoSocket(t *testing.T) {
	t.Setenv(notifySocketEnv, "")
	sent, err := SdNotify(NotifyReady)
	require.NoError(t, err)
	assert.False(t, sent)
}
