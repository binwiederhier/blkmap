package cow

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"heckel.io/blkmap/source"
)

// With elision on, writes that repeat what the device already reads are dropped: no chunk
// is recorded for an identical write over the base, an identical rewrite of a stored chunk
// does not dirty the store, and zeroing a range the base already reads as zeros is free.
func TestElideIdenticalWrites(t *testing.T) {
	dir := t.TempDir()
	base := &mem{data: pattern(testSize)}
	s, err := Open(base, filepath.Join(dir, "cow"), filepath.Join(dir, "cow.bitmap"), testChunk)
	require.NoError(t, err)
	defer s.Close()
	s.SetElision(true)

	// identical partial write over the base: nothing stored
	_, err = s.WriteAt(base.data[100:600], 100)
	require.NoError(t, err)
	require.EqualValues(t, 0, s.Written())
	require.False(t, s.Dirty())

	// identical whole-chunk write over the base: nothing stored
	_, err = s.WriteAt(base.data[testChunk:2*testChunk], testChunk)
	require.NoError(t, err)
	require.EqualValues(t, 0, s.Written())

	// a different write is stored, and the rest of its chunk is copied up as usual
	changed := bytes.Repeat([]byte{0xAB}, 300)
	_, err = s.WriteAt(changed, 2*testChunk+50)
	require.NoError(t, err)
	require.EqualValues(t, 1, s.Written())
	got := make([]byte, testChunk)
	_, err = s.ReadAt(got, 2*testChunk)
	require.NoError(t, err)
	require.Equal(t, base.data[2*testChunk:2*testChunk+50], got[:50])
	require.Equal(t, changed, got[50:350])
	require.Equal(t, base.data[2*testChunk+350:3*testChunk], got[350:])
	require.NoError(t, s.Flush())

	// rewriting a stored chunk with what it already holds does not dirty the store
	_, err = s.WriteAt(changed, 2*testChunk+50)
	require.NoError(t, err)
	require.False(t, s.Dirty())
	// but a real change to it does
	_, err = s.WriteAt([]byte{1, 2, 3}, 2*testChunk+50)
	require.NoError(t, err)
	require.True(t, s.Dirty())

	// zeroing a chunk the base reads as zeros records nothing; zeroing data does
	zeroBase := &mem{data: make([]byte, testSize)}
	copy(zeroBase.data[5*testChunk:], pattern(testChunk))
	z, err := Open(zeroBase, filepath.Join(dir, "z"), filepath.Join(dir, "z.bitmap"), testChunk)
	require.NoError(t, err)
	defer z.Close()
	z.SetElision(true)
	require.NoError(t, z.WriteZeroes(0, testChunk))
	require.EqualValues(t, 0, z.Written())
	require.NoError(t, z.WriteZeroes(5*testChunk, testChunk))
	require.EqualValues(t, 1, z.Written())
	_, err = z.ReadAt(got, 5*testChunk)
	require.NoError(t, err)
	require.Equal(t, make([]byte, testChunk), got)

	// with elision off, the same identical write is stored
	s.SetElision(false)
	_, err = s.WriteAt(base.data[7*testChunk:7*testChunk+10], 7*testChunk)
	require.NoError(t, err)
	require.EqualValues(t, 2, s.Written())
}

// derived is a base whose bytes are computed from other devices (a source.Binder): they can
// change after a write was checked against them.
type derived struct {
	data []byte
}

func (d *derived) ReadAt(p []byte, off int64) (int, error) { return copy(p, d.data[off:]), nil }
func (d *derived) Size() int64                             { return int64(len(d.data)) }
func (d *derived) Close() error                            { return nil }
func (d *derived) Bind(source.Lookup)                      {}

// Over a base that can change, a write must not be elided against it: the guest's bytes
// would silently turn into whatever the base derives next.
func TestElisionNeverTrustsAChangingBase(t *testing.T) {
	dir := t.TempDir()
	base := &derived{data: make([]byte, testSize)}
	s, err := Open(base, filepath.Join(dir, "cow"), filepath.Join(dir, "cow.bitmap"), testChunk)
	require.NoError(t, err)
	defer s.Close()
	s.SetElision(true)
	_, err = s.WriteAt(make([]byte, testChunk), 0) // equal to what the base derives right now
	require.NoError(t, err)
	require.NoError(t, s.WriteZeroes(testChunk, testChunk))
	base.data[10], base.data[testChunk+10] = 0xFF, 0xFF // a sibling changes
	got := make([]byte, 2*testChunk)
	_, err = s.ReadAt(got, 0)
	require.NoError(t, err)
	require.Equal(t, make([]byte, 2*testChunk), got, "the device must keep what the guest wrote")
	// Rewriting a stored chunk with its own bytes is still elided: that does not depend on the base
	require.NoError(t, s.Flush())
	_, err = s.WriteAt(make([]byte, 100), 0)
	require.NoError(t, err)
	require.False(t, s.Dirty())
}

// A whole-chunk write does not need the base, so elision must not make it fail when the base
// cannot be read: the write is stored instead.
func TestElisionSurvivesAnUnreadableBase(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(&broken{size: testSize}, filepath.Join(dir, "cow"), filepath.Join(dir, "cow.bitmap"), testChunk)
	require.NoError(t, err)
	defer s.Close()
	s.SetElision(true)
	w := bytes.Repeat([]byte{0x5A}, testChunk)
	_, err = s.WriteAt(w, testChunk)
	require.NoError(t, err)
	require.EqualValues(t, 1, s.Written())
	got := make([]byte, testChunk)
	_, err = s.ReadAt(got, testChunk)
	require.NoError(t, err)
	require.Equal(t, w, got)
}
