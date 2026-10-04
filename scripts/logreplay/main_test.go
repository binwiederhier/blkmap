package main

import (
	"bytes"
	"encoding/binary"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testLog writes a dm-log-writes log with 512-byte sectors: entries are (sector, flags, data
// or mark) in log order.
func testLog(t *testing.T, entries []testEntry) *os.File {
	t.Helper()
	const ss = 512
	var b bytes.Buffer
	super := make([]byte, ss)
	binary.LittleEndian.PutUint64(super[0:], logMagic)
	binary.LittleEndian.PutUint64(super[8:], logVersion)
	binary.LittleEndian.PutUint64(super[16:], uint64(len(entries)))
	binary.LittleEndian.PutUint32(super[24:], ss)
	b.Write(super)
	for _, e := range entries {
		hdr := make([]byte, ss)
		binary.LittleEndian.PutUint64(hdr[0:], e.sector)
		binary.LittleEndian.PutUint64(hdr[8:], uint64(len(e.data)/ss))
		binary.LittleEndian.PutUint64(hdr[16:], e.flags)
		if e.mark != "" {
			binary.LittleEndian.PutUint64(hdr[24:], uint64(len(e.mark)))
			copy(hdr[entrySize:], e.mark)
		}
		b.Write(hdr)
		b.Write(e.data)
	}
	path := filepath.Join(t.TempDir(), "log")
	require.NoError(t, os.WriteFile(path, b.Bytes(), 0600))
	f, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { f.Close() })
	return f
}

type testEntry struct {
	sector uint64
	flags  uint64
	data   []byte
	mark   string
}

func sectors(c byte, n int) []byte {
	return bytes.Repeat([]byte{c}, n*512)
}

func TestReadLogAndApply(t *testing.T) {
	log := testLog(t, []testEntry{
		{sector: 0, data: sectors('a', 2)},
		{flags: flagFlush},
		{mark: "ack15", flags: flagMark},
		{sector: 4, flags: flagFUA, data: sectors('b', 1)},
		{sector: 1, data: sectors('c', 1)},
		{flags: flagFlush},
	})
	ss, entries, err := readLog(log)
	require.NoError(t, err)
	assert.Equal(t, int64(512), ss)
	require.Len(t, entries, 6)
	assert.Equal(t, "ack15", entries[2].mark)
	assert.Equal(t, uint64(1), entries[4].sector)

	target := filepath.Join(t.TempDir(), "target")
	require.NoError(t, os.WriteFile(target, make([]byte, 8*512), 0600))
	f, err := os.OpenFile(target, os.O_RDWR, 0)
	require.NoError(t, err)
	defer f.Close()
	// Everything up to the first flush, then the FUA write but none of the plain ones
	require.NoError(t, apply(log, f, ss, entries, 0, 2, 1, nil))
	require.NoError(t, apply(log, f, ss, entries, 2, 5, 0, rand.New(rand.NewPCG(1, 2))))
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	want := append(append(append(sectors('a', 2), sectors(0, 2)...), sectors('b', 1)...), sectors(0, 3)...)
	assert.Equal(t, want, got, "the FUA write survives, the unflushed plain write is dropped")
}

func TestPlan(t *testing.T) {
	entries := []entry{
		{flags: 0}, {flags: flagFlush}, {flags: flagMark, mark: "ack15"}, {flags: flagFUA},
		{flags: 0}, {flags: flagFlush}, {flags: flagMark, mark: "ack31"}, {flags: flagFlush | flagFUA},
	}
	assert.Equal(t, []checkpoint{{q: 1, p: -1, ack: -1}, {q: 5, p: 1, ack: 15}, {q: 7, p: 5, ack: 31}}, plan(entries, 1))
	assert.Equal(t, []checkpoint{{q: 1, p: -1, ack: -1}, {q: 7, p: 5, ack: 31}}, plan(entries, 2))
}
