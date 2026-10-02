package device

import (
	"bytes"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"heckel.io/blkmap/cow"
	"heckel.io/blkmap/source"
)

const groupChunk = 4096

func seeded(n int, seed byte) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = seed + byte(i*7)
	}
	return p
}

// bytesSource is an in-memory read-only base.
type bytesSource struct{ data []byte }

func (b *bytesSource) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(b.data)) {
		return 0, io.EOF
	}
	n := copy(p, b.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
func (b *bytesSource) Size() int64  { return int64(len(b.data)) }
func (b *bytesSource) Close() error { return nil }

func openTestStore(t *testing.T, dir, id string, data []byte) *cow.Store {
	t.Helper()
	s, err := cow.Open(&bytesSource{data: data}, filepath.Join(dir, id+".cow"), filepath.Join(dir, id+".bitmap"), groupChunk)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

// Device b aliases its second and third chunks onto a's fifth and sixth: reads there show
// a's bytes, writes through either device land in a's store once, and b's own ranges are
// untouched. Flushing b flushes a too.
func TestRouterAliases(t *testing.T) {
	dir := t.TempDir()
	a := openTestStore(t, dir, "a", seeded(8*groupChunk, 1))
	b := openTestStore(t, dir, "b", seeded(8*groupChunk, 2))
	ra, rb := &router{id: "a", store: a}, &router{id: "b", store: b}
	routers := map[string]*router{"a": ra, "b": rb}
	require.NoError(t, ra.setAliases(nil, routers))
	require.NoError(t, rb.setAliases([]Alias{{Offset: groupChunk, Length: 2 * groupChunk, Target: "a", TargetOffset: 4 * groupChunk}}, routers))

	// a read across own range, alias, own range
	got := make([]byte, 4*groupChunk)
	_, err := rb.ReadAt(got, 0)
	require.NoError(t, err)
	require.Equal(t, seeded(8*groupChunk, 2)[:groupChunk], got[:groupChunk], "own range before the alias")
	require.Equal(t, seeded(8*groupChunk, 1)[4*groupChunk:6*groupChunk], got[groupChunk:3*groupChunk], "aliased range shows a")
	require.Equal(t, seeded(8*groupChunk, 2)[3*groupChunk:4*groupChunk], got[3*groupChunk:], "own range after the alias")

	// a write through b into the alias lands in a's store and is visible through both
	w := bytes.Repeat([]byte{0xEE}, 100)
	_, err = rb.WriteAt(w, groupChunk+10)
	require.NoError(t, err)
	require.EqualValues(t, 0, b.Written(), "nothing stored in b for an aliased write")
	require.EqualValues(t, 1, a.Written())
	back := make([]byte, 100)
	_, err = ra.ReadAt(back, 4*groupChunk+10)
	require.NoError(t, err)
	require.Equal(t, w, back)
	_, err = rb.ReadAt(back, groupChunk+10)
	require.NoError(t, err)
	require.Equal(t, w, back)

	// a write through a is visible through b's alias
	w2 := bytes.Repeat([]byte{0x11}, 50)
	_, err = ra.WriteAt(w2, 5*groupChunk)
	require.NoError(t, err)
	_, err = rb.ReadAt(back[:50], 2*groupChunk)
	require.NoError(t, err)
	require.Equal(t, w2, back[:50])

	// discard through the alias punches a's chunk, zeroes through the alias zero a
	require.NoError(t, rb.Discard(groupChunk, groupChunk))
	_, err = ra.ReadAt(back, 4*groupChunk+10)
	require.NoError(t, err)
	require.Equal(t, make([]byte, 100), back)
	require.NoError(t, rb.WriteZeroes(2*groupChunk, groupChunk))
	_, err = ra.ReadAt(back[:50], 5*groupChunk)
	require.NoError(t, err)
	require.Equal(t, make([]byte, 50), back[:50])

	// flushing b flushes a as well
	_, err = ra.WriteAt([]byte{7}, 7*groupChunk)
	require.NoError(t, err)
	require.True(t, a.Dirty())
	require.NoError(t, rb.Flush())
	require.False(t, a.Dirty())
}

func TestRouterAliasValidation(t *testing.T) {
	dir := t.TempDir()
	a := openTestStore(t, dir, "a", make([]byte, 4*groupChunk))
	b := openTestStore(t, dir, "b", make([]byte, 4*groupChunk))
	ra, rb := &router{id: "a", store: a}, &router{id: "b", store: b}
	routers := map[string]*router{"a": ra, "b": rb}
	for name, aliases := range map[string][]Alias{
		"outside device":  {{Offset: 3 * groupChunk, Length: 2 * groupChunk, Target: "a"}},
		"unknown target":  {{Offset: 0, Length: groupChunk, Target: "c"}},
		"self":            {{Offset: 0, Length: groupChunk, Target: "b"}},
		"outside target":  {{Offset: 0, Length: groupChunk, Target: "a", TargetOffset: 4 * groupChunk}},
		"overlapping":     {{Offset: 0, Length: 2 * groupChunk, Target: "a"}, {Offset: groupChunk, Length: groupChunk, Target: "a"}},
		"negative length": {{Offset: 0, Length: 0, Target: "a"}},
	} {
		require.Error(t, rb.setAliases(aliases, routers), name)
	}
}

// A base that implements Binder gets a lookup of its siblings' live views and reads them
// through their COW stores, so it sees what the guest wrote.
type siblingSource struct {
	lookup source.Lookup
}

func (s *siblingSource) Bind(lookup source.Lookup) { s.lookup = lookup }
func (s *siblingSource) ReadAt(p []byte, off int64) (int, error) {
	a, ok := s.lookup("a")
	if !ok {
		return 0, io.ErrUnexpectedEOF
	}
	return a.ReadAt(p, off)
}
func (s *siblingSource) Size() int64  { return 8 * groupChunk }
func (s *siblingSource) Close() error { return nil }

func TestBinderSeesSiblingWrites(t *testing.T) {
	dir := t.TempDir()
	a := openTestStore(t, dir, "a", seeded(8*groupChunk, 1))
	src := &siblingSource{}
	b, err := cow.Open(src, filepath.Join(dir, "b.cow"), filepath.Join(dir, "b.bitmap"), groupChunk)
	require.NoError(t, err)
	defer b.Close()
	ra, rb := &router{id: "a", store: a}, &router{id: "b", store: b}
	routers := map[string]*router{"a": ra, "b": rb}
	src.Bind(func(id string) (io.ReaderAt, bool) { r, ok := routers[id]; return r, ok })
	_, err = ra.WriteAt([]byte{9, 9, 9}, 100)
	require.NoError(t, err)
	got := make([]byte, 3)
	_, err = rb.ReadAt(got, 100)
	require.NoError(t, err)
	require.Equal(t, []byte{9, 9, 9}, got, "b's base reads a through its store, overlay included")
}
