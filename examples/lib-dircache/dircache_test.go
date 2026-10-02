package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"heckel.io/blkmap/source"
)

func TestDirCache(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	slowPath := filepath.Join(dir, "slow.img")
	data := make([]byte, 3*blockSize+4096)
	for i := range data {
		data[i] = byte(i * 7)
	}
	require.NoError(t, os.WriteFile(slowPath, data, 0600))
	slow, err := source.OpenFile(slowPath, 0, 0)
	require.NoError(t, err)
	cacheDir := filepath.Join(dir, "cache")
	require.NoError(t, os.Mkdir(cacheDir, 0755))
	fast := NewDirCache(cacheDir, slow.Size())
	// Nothing cached: every read is a miss served by the slow tier
	c := source.NewCache(fast, slow)
	p := make([]byte, 8192)
	_, err = c.ReadAt(p, blockSize-4096)
	require.NoError(t, err)
	assert.Equal(t, data[blockSize-4096:blockSize+4096], p)
	assert.Equal(t, source.CacheStats{Misses: 1}, c.Stats())
	// Block 1 cached, block 0 not: a read inside block 1 hits, one straddling 0 and 1 misses
	require.NoError(t, fast.Populate(slow, 1, 2))
	_, err = c.ReadAt(p, blockSize+100)
	require.NoError(t, err)
	assert.Equal(t, data[blockSize+100:blockSize+100+8192], p)
	_, err = c.ReadAt(p, blockSize-4096)
	require.NoError(t, err)
	assert.Equal(t, source.CacheStats{Hits: 1, Misses: 2}, c.Stats())
	// The partial last block works too, and a corrupt (short) block file is a miss
	require.NoError(t, fast.Populate(slow, 3, 4))
	_, err = c.ReadAt(p[:4096], 3*blockSize)
	require.NoError(t, err)
	assert.Equal(t, data[3*blockSize:], p[:4096])
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, "00000001.blk"), []byte("short"), 0600))
	_, err = c.ReadAt(p, blockSize+100)
	require.NoError(t, err)
	assert.Equal(t, data[blockSize+100:blockSize+100+8192], p)
	assert.Equal(t, source.CacheStats{Hits: 2, Misses: 3}, c.Stats())
	// A garbage block file is detected only by content, not by blkmap: the cache is trusted
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, "00000002.blk"), bytes.Repeat([]byte{1}, blockSize), 0600))
	_, err = c.ReadAt(p[:16], 2*blockSize)
	require.NoError(t, err)
	assert.Equal(t, bytes.Repeat([]byte{1}, 16), p[:16])
}
