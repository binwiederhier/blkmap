package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecordRoundTrip(t *testing.T) {
	b := encode(42, 7)
	seq, slot, ok := decode(b)
	require.True(t, ok)
	assert.Equal(t, uint64(42), seq)
	assert.Equal(t, uint64(7), slot)
	b[100] ^= 1
	_, _, ok = decode(b)
	assert.False(t, ok, "a torn record fails its checksum")
	_, _, ok = decode(make([]byte, recordSize))
	assert.False(t, ok)
}

// image is a device file for the verifier.
func image(t *testing.T, slots uint64, seqs ...uint64) string {
	path := filepath.Join(t.TempDir(), "dev")
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(int64(slots*recordSize)))
	for _, seq := range seqs {
		_, err := f.WriteAt(encode(seq, slotOf(seq, slots)), int64(slotOf(seq, slots)*recordSize))
		require.NoError(t, err)
	}
	require.NoError(t, f.Close())
	return path
}

func TestVerify(t *testing.T) {
	const slots = 64
	all := make([]uint64, 200)
	for i := range all {
		all[i] = uint64(i)
	}
	ranges := []span{{0, 199}}
	// Everything acknowledged is there
	require.NoError(t, verify(image(t, slots, all...), "", ranges))
	// An acknowledged write that reverted (the slot shows an older record) is a failure
	lost := image(t, slots, all[:150]...)
	assert.Error(t, verify(lost, "", ranges))
	// Writes past the last acknowledgment may be lost
	require.NoError(t, verify(image(t, slots, all[:190]...), "", []span{{0, 180}}))
	// Ranges of earlier cycles: a cycle that restarted at seq 100 after its predecessor
	// acknowledged up to 90 (91..99 lost or present) still verifies
	require.NoError(t, verify(image(t, slots, append(all[:95], all[100:]...)...), "", []span{{0, 90}, {100, 199}}))
	// A slot holding garbage where an acknowledged record belongs is a failure
	path := image(t, slots, all...)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte("garbage"), int64(slotOf(199, slots)*recordSize+50))
	require.NoError(t, err)
	f.Close()
	assert.Error(t, verify(path, "", ranges))
}

func TestParseRanges(t *testing.T) {
	r, err := parseSpans([]string{"0-10", "20-30"})
	require.NoError(t, err)
	assert.Equal(t, []span{{0, 10}, {20, 30}}, r)
	_, err = parseSpans([]string{"x"})
	assert.Error(t, err)
}

func TestVerifyAgainstBase(t *testing.T) {
	const slots = 64
	base := filepath.Join(t.TempDir(), "base")
	content := make([]byte, slots*recordSize)
	for i := range content {
		content[i] = byte(i*13 + i/4096)
	}
	require.NoError(t, os.WriteFile(base, content, 0600))
	// The device is the base with records 0..39 on top; 0..31 were acknowledged
	dev := func() string {
		path := filepath.Join(t.TempDir(), "dev")
		require.NoError(t, os.WriteFile(path, content, 0600))
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		require.NoError(t, err)
		defer f.Close()
		for seq := uint64(0); seq < 40; seq++ {
			_, err := f.WriteAt(encode(seq, slotOf(seq, slots)), int64(slotOf(seq, slots)*recordSize))
			require.NoError(t, err)
		}
		return path
	}
	acked := []span{{0, 31}}
	require.NoError(t, verify(dev(), base, acked))
	// A slot no record touched that no longer reads as the base: a copy-up was lost
	path := dev()
	untouched := map[uint64]bool{}
	for seq := uint64(0); seq < 64; seq++ {
		untouched[slotOf(seq, slots)] = true
	}
	var free uint64
	for free = 0; untouched[free]; free++ {
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = f.WriteAt(make([]byte, recordSize), int64(free*recordSize))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	assert.Error(t, verify(path, base, acked))
	assert.NoError(t, verify(path, "", acked), "without a base only records are checked")
}
