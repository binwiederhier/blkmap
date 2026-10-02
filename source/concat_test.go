package source

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mem is an in-memory Source for layout tests.
type mem struct {
	data   []byte
	closed bool
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
	m.closed = true
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

func TestConcatHolesAndDirect(t *testing.T) {
	t.Parallel()
	fast := &tier{mem: mem{data: filled(100, 'F').data}}
	slow := &tier{mem: mem{data: filled(100, 'S').data}}
	c, err := NewConcat([]*Segment{
		{Offset: 0, Source: NewZero(50)},
		{Offset: 50, Source: filled(100, 'a')},
		{Offset: 200, Source: NewCache(fast, slow)}, // gap 150..200
		{Offset: 300, Source: NewZero(10)},
	}, 400) // tail 310..400
	require.NoError(t, err)
	holes, err := Holes(c, 0, 400)
	require.NoError(t, err)
	assert.Equal(t, []Range{{0, 50}, {150, 50}, {300, 10}, {310, 90}}, holes)
	// A window clips and skips
	holes, err = Holes(c, 40, 120)
	require.NoError(t, err)
	assert.Equal(t, []Range{{40, 10}, {150, 10}}, holes)
	holes, err = Holes(c, 60, 50)
	require.NoError(t, err)
	assert.Empty(t, holes)
	holes, err = Holes(filled(10, 'x'), 0, 10)
	require.NoError(t, err)
	assert.Nil(t, holes) // unknown for plain sources
	p := make([]byte, 100)
	_, err = c.ReadAt(p, 200)
	require.NoError(t, err)
	assert.Equal(t, filled(100, 'F').data, p)
	_, err = ReadDirect(c, p, 200)
	require.NoError(t, err)
	assert.Equal(t, filled(100, 'S').data, p)
	// Direct reads of non-cache segments behave like plain reads
	_, err = ReadDirect(c, p[:50], 50)
	require.NoError(t, err)
	assert.Equal(t, filled(50, 'a').data, p[:50])
}

func TestConcatManySegmentsNoAlloc(t *testing.T) {
	// 2000 tiny segments: locate must not be linear and ReadAt must not allocate
	var segs []*Segment
	for i := 0; i < 2000; i++ {
		segs = append(segs, &Segment{Offset: int64(i) * 16, Source: filled(16, byte('a'+i%26))})
	}
	c, err := NewConcat(segs, 0)
	require.NoError(t, err)
	p := make([]byte, 64)
	assert.Zero(t, testing.AllocsPerRun(100, func() {
		if _, err := c.ReadAt(p, 31_000); err != nil {
			t.Fatal(err)
		}
	}))
	assert.Equal(t, filled(16, byte('a'+1937%26)).data[8:], p[:8])
}

func BenchmarkConcatReadAt(b *testing.B) {
	for _, n := range []int{1, 10, 1000} {
		var segs []*Segment
		for i := 0; i < n; i++ {
			segs = append(segs, &Segment{Offset: int64(i) << 20, Source: filled(1<<20, 'x')})
		}
		c, _ := NewConcat(segs, 0)
		p := make([]byte, 4096)
		b.Run(fmt.Sprintf("segments=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(p)))
			for i := 0; i < b.N; i++ {
				c.ReadAt(p, int64(i%n)<<20+1234)
			}
		})
	}
}
