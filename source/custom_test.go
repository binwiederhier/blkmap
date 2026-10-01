package source

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"heckel.io/blkmap/config"
)

func TestCustom(t *testing.T) {
	Register("test-fill", func(size int64, params map[string]string) (Source, error) {
		return filled(int(size), params["byte"][0]), nil
	})
	src, err := NewCustom("test-fill", 1024, map[string]string{"byte": "q"})
	require.NoError(t, err)
	assert.Equal(t, int64(1024), src.Size())
	_, err = NewCustom("nope", 0, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"nope"`)
	// Through a config
	c, err := config.Parse("d", []byte("segments:\n  - type: custom\n    name: test-fill\n    size: 2K\n    params:\n      byte: z\n"))
	require.NoError(t, err)
	base, err := FromConfig(c)
	require.NoError(t, err)
	p := make([]byte, 2048)
	_, err = base.ReadAt(p, 0)
	require.NoError(t, err)
	assert.Equal(t, filled(2048, 'z').data, p)
}
