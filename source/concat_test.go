package source

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mem is an in-memory Source for layout tests.
type mem struct {
	data []byte
}

func (m *mem) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(m.data)) {
		return 0, io.EOF
	}
	n := copy(p, m.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m *mem) Size() int64 {
	return int64(len(m.data))
}

func (m *mem) Close() error {
	return nil
}

func filled(n int, b byte) *mem {
	return &mem{data: bytes.Repeat([]byte{b}, n)}
}

func TestConcat(t *testing.T) {
	t.Parallel()
	c, err := NewConcat([]*Segment{
		{Offset: 0, Source: filled(100, 'a')},
		{Offset: 100, Source: filled(50, 'b')},
		{Offset: 200, Source: filled(100, 'c')}, // gap 150..200
	}, 400) // tail 300..400
	require.NoError(t, err)
	assert.Equal(t, int64(400), c.Size())
	expected := append(append(append(append(
		bytes.Repeat([]byte{'a'}, 100),
		bytes.Repeat([]byte{'b'}, 50)...),
		bytes.Repeat([]byte{0}, 50)...),
		bytes.Repeat([]byte{'c'}, 100)...),
		bytes.Repeat([]byte{0}, 100)...)
	// Whole thing
	p := make([]byte, 400)
	n, err := c.ReadAt(p, 0)
	require.NoError(t, err)
	assert.Equal(t, 400, n)
	assert.Equal(t, expected, p)
	// Every window, to exercise all boundaries
	for off := 0; off < 400; off += 7 {
		for size := 1; size < 130 && off+size <= 400; size += 13 {
			p := make([]byte, size)
			n, err := c.ReadAt(p, int64(off))
			require.NoError(t, err, "off=%d size=%d", off, size)
			assert.Equal(t, size, n)
			assert.Equal(t, expected[off:off+size], p, "off=%d size=%d", off, size)
		}
	}
	// Past the end
	p = make([]byte, 10)
	n, err = c.ReadAt(p, 395)
	assert.ErrorIs(t, err, io.EOF)
	assert.Equal(t, 5, n)
	n, err = c.ReadAt(p, 400)
	assert.ErrorIs(t, err, io.EOF)
	assert.Equal(t, 0, n)
	require.NoError(t, c.Close())
}

func TestConcatImplicitSize(t *testing.T) {
	t.Parallel()
	c, err := NewConcat([]*Segment{{Offset: 10, Source: filled(100, 'a')}}, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(110), c.Size())
	assert.Len(t, c.Segments(), 1)
}

func TestConcatErrors(t *testing.T) {
	t.Parallel()
	_, err := NewConcat(nil, 100)
	require.Error(t, err)
	_, err = NewConcat([]*Segment{{Offset: 0, Source: filled(100, 'a')}, {Offset: 50, Source: filled(10, 'b')}}, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "overlap")
	_, err = NewConcat([]*Segment{{Offset: 100, Source: filled(10, 'a')}, {Offset: 0, Source: filled(10, 'b')}}, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "overlap")
	_, err = NewConcat([]*Segment{{Offset: 0, Source: filled(100, 'a')}}, 50)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")
	_, err = NewConcat([]*Segment{{Offset: 0, Source: filled(0, 'a')}}, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}
