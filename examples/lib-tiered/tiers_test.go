package main

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"heckel.io/blkmap/source"
)

func image(t *testing.T, size int) (*source.File, []byte) {
	t.Helper()
	data := make([]byte, size)
	_, _ = rand.Read(data)
	path := filepath.Join(t.TempDir(), "img")
	require.NoError(t, os.WriteFile(path, data, 0600))
	f, err := source.OpenFile(path, 0, 0)
	require.NoError(t, err)
	t.Cleanup(func() { f.Close() })
	return f, data
}

func TestTieredReadsAndStats(t *testing.T) {
	file, data := image(t, 8<<20)
	slow := newSlowTier(file, 5*time.Millisecond, "1")
	fast := newMemTier(slow.Size())
	require.NoError(t, fast.fill(file, 0, 4))
	c := tiered(fast, slow)
	got := make([]byte, 8<<20)
	_, err := c.ReadAt(got, 0)
	require.NoError(t, err)
	assert.Equal(t, data, got)
	st := c.Stats()
	assert.Equal(t, int64(1), st.Misses, "the read straddling cached and uncached blocks is one miss")
	start := time.Now()
	p := make([]byte, 4096)
	_, err = c.ReadAt(p, 1<<20)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 5*time.Millisecond, "a cached block does not pay the round trip")
	assert.Equal(t, data[1<<20:1<<20+4096], p)
}

func TestSlowTierAbortUnblocks(t *testing.T) {
	file, _ := image(t, 1<<20)
	slow := newSlowTier(file, time.Hour, "1")
	done := make(chan error, 1)
	go func() {
		_, err := slow.ReadAt(make([]byte, 10), 0)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	source.Abort(tiered(newMemTier(slow.Size()), slow)) // reaches the slow tier through the cache and read-ahead
	select {
	case err := <-done:
		assert.ErrorIs(t, err, errAborted)
	case <-time.After(time.Second):
		t.Fatal("abort did not reach the slow tier")
	}
}
