package util

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBitset(t *testing.T) {
	b := NewBitset(130)
	assert.Equal(t, int64(130), b.Len())
	assert.False(t, b.Test(0))
	b.Set(0)
	b.Set(63)
	b.Set(64)
	b.Set(129)
	assert.True(t, b.Test(0))
	assert.True(t, b.Test(63))
	assert.True(t, b.Test(64))
	assert.True(t, b.Test(129))
	assert.False(t, b.Test(1))
	assert.False(t, b.Test(128))
	assert.Zero(t, testing.AllocsPerRun(100, func() { b.Set(5); b.Test(5) }))
}
