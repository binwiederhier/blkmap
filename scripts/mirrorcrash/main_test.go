package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestJournalResyncOfAnOlderWrite(t *testing.T) {
	// m1 gets seq 139 alone, m0 then gets seq 140 for the same slot, and only later the resync
	// copy of 139: m0 must hold 139, the write it got last
	j := parseJournal(`W 1 7 139
A 1 1 7 139
W 0 7 140
A 2 0 7 140
W 0 7 139
A 3 0 7 139
`, 3)
	assert.False(t, j.lost(0, 7, 139, true), "the resync copy is m0's newest acknowledged write")
	assert.True(t, j.lost(0, 7, 140, true), "140 was overwritten by the acknowledged resync")
	assert.False(t, j.lost(1, 7, 139, true))
	// Up to flush 2 the resync is not acknowledged: 140, or the unacknowledged 139 after it
	j = parseJournal(`W 1 7 139
A 1 1 7 139
W 0 7 140
A 2 0 7 140
W 0 7 139
A 3 0 7 139
`, 2)
	assert.False(t, j.lost(0, 7, 140, true))
	assert.False(t, j.lost(0, 7, 139, true), "the resync copy was written after 140, just not acknowledged")
	assert.True(t, j.lost(0, 7, 5, true), "a write before 140 means 140 was lost")
	assert.True(t, j.lost(0, 7, 0, false))
}
