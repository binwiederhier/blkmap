package source

import (
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestZero(t *testing.T) {
	t.Parallel()
	z := NewZero(1024)
	assert.Equal(t, int64(1024), z.Size())
	p := []byte{1, 2, 3, 4}
	n, err := z.ReadAt(p, 100)
	require.NoError(t, err)
	assert.Equal(t, 4, n)
	assert.Equal(t, []byte{0, 0, 0, 0}, p)
	p = []byte{1, 2, 3, 4}
	n, err = z.ReadAt(p, 1022)
	assert.ErrorIs(t, err, io.EOF)
	assert.Equal(t, 2, n)
	assert.Equal(t, []byte{0, 0, 3, 4}, p)
	n, err = z.ReadAt(p, 2000)
	assert.ErrorIs(t, err, io.EOF)
	assert.Equal(t, 0, n)
	require.NoError(t, z.Close())
}
