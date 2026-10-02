package device

import (
	"bytes"
	"context"
	"io"
	"os"
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
		"outside device": {{Offset: 3 * groupChunk, Length: 2 * groupChunk, Target: "a"}},
		"unknown target": {{Offset: 0, Length: groupChunk, Target: "c"}},
		"self":           {{Offset: 0, Length: groupChunk, Target: "b"}},
		"outside target": {{Offset: 0, Length: groupChunk, Target: "a", TargetOffset: 4 * groupChunk}},
		"overlapping":    {{Offset: 0, Length: 2 * groupChunk, Target: "a"}, {Offset: groupChunk, Length: groupChunk, Target: "a"}},
		"empty":          {{Offset: 0, Length: 0, Target: "a"}},
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

// An alias whose target range is itself aliased would forward again; a pair aliasing each
// other forwards forever. Both are refused.
func TestRouterRefusesAliasChains(t *testing.T) {
	dir := t.TempDir()
	a := openTestStore(t, dir, "a", make([]byte, 4*groupChunk))
	b := openTestStore(t, dir, "b", make([]byte, 4*groupChunk))
	c := openTestStore(t, dir, "c", make([]byte, 4*groupChunk))
	ra, rb, rc := &router{id: "a", store: a}, &router{id: "b", store: b}, &router{id: "c", store: c}
	routers := map[string]*router{"a": ra, "b": rb, "c": rc}
	opts := []*GroupOptions{
		{Options: Options{ID: "a"}, Aliases: []Alias{{Offset: 0, Length: groupChunk, Target: "b"}}},
		{Options: Options{ID: "b"}, Aliases: []Alias{{Offset: 0, Length: groupChunk, Target: "a"}}},
	}
	require.Error(t, setAllAliases(opts, routers), "a cycle")
	opts = []*GroupOptions{
		{Options: Options{ID: "a"}, Aliases: []Alias{{Offset: 0, Length: groupChunk, Target: "b", TargetOffset: 2 * groupChunk}}},
		{Options: Options{ID: "b"}, Aliases: []Alias{{Offset: 2 * groupChunk, Length: groupChunk, Target: "c"}}},
		{Options: Options{ID: "c"}},
	}
	require.Error(t, setAllAliases(opts, routers), "a chain")
	// Aliasing a range of the target next to (not inside) its own alias is fine
	opts[0].Aliases[0].TargetOffset = groupChunk
	require.NoError(t, setAllAliases(opts, routers))
}

// Bases are usually stitched from parts (a config always yields a Concat): every Binder in
// the tree gets the lookup, not only a Binder at the top.
func TestBindReachesNestedSources(t *testing.T) {
	nested := &siblingSource{}
	concat, err := source.NewConcat([]*source.Segment{{Offset: 0, Source: nested}}, 0)
	require.NoError(t, err)
	top := &siblingSource{}
	bindAll([]source.Source{concat, top}, func(string) (io.ReaderAt, bool) { return nil, false })
	require.NotNil(t, nested.lookup)
	require.NotNil(t, top.lookup)
}

// A cancelled context returns before anything is opened, so a restart that is stopped while
// starting leaves a predecessor waiting for recovery alone.
func TestServeCancelledTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	base := &bytesSource{data: make([]byte, 1<<20)}
	_, err := Serve(ctx, &Options{ID: "cx", Base: base, COWFile: filepath.Join(dir, "cx.cow"), RunDir: dir, DevDir: dir})
	require.ErrorIs(t, err, context.Canceled)
	_, statErr := os.Stat(filepath.Join(dir, "cx.cow"))
	require.True(t, os.IsNotExist(statErr), "the cow file must not even be created")
	_, err = ServeGroup(ctx, []*GroupOptions{{Options: Options{ID: "cy", Base: base, COWFile: filepath.Join(dir, "cy.cow"), RunDir: dir, DevDir: dir}}})
	require.ErrorIs(t, err, context.Canceled)
	_, statErr = os.Stat(filepath.Join(dir, "cy.cow"))
	require.True(t, os.IsNotExist(statErr))
}

// countingBase counts Close calls.
type countingBase struct {
	bytesSource
	closes int
}

func (c *countingBase) Close() error {
	c.closes++
	return nil
}

// Through the kernel: b's second and third chunks are a's sixth and seventh. b is listed
// first, so it comes up before its alias target, and writes through b's page cache are only
// written out when b stops; the group must still have a's store open then.
func TestServeGroupThroughKernel(t *testing.T) {
	requireUblk(t)
	dir := t.TempDir()
	opt := func(id string, data []byte) Options {
		return Options{ID: id, Base: &bytesSource{data: data}, COWFile: filepath.Join(dir, id+".cow"), ChunkSize: groupChunk,
			DevDir: filepath.Join(dir, "dev"), RunDir: filepath.Join(dir, "run")}
	}
	g, err := ServeGroup(context.Background(), []*GroupOptions{
		{Options: opt("gb", seeded(8*groupChunk, 2)), Aliases: []Alias{{Offset: groupChunk, Length: 2 * groupChunk, Target: "ga", TargetOffset: 5 * groupChunk}}},
		{Options: opt("ga", seeded(8*groupChunk, 1))},
	})
	require.NoError(t, err)
	// The aliased range of b shows a's bytes
	fb, err := os.OpenFile(g.Devices["gb"].BlockPath, os.O_RDWR, 0)
	require.NoError(t, err)
	got := make([]byte, groupChunk)
	_, err = fb.ReadAt(got, groupChunk)
	require.NoError(t, err)
	require.Equal(t, seeded(8*groupChunk, 1)[5*groupChunk:6*groupChunk], got)
	// A buffered write through b, never flushed by us
	w := bytes.Repeat([]byte{0xC3}, groupChunk)
	_, err = fb.WriteAt(w, 2*groupChunk)
	require.NoError(t, err)
	require.NoError(t, fb.Close())
	require.NoError(t, g.Close())
	// It reached a's store
	a, err := cow.Open(&bytesSource{data: seeded(8*groupChunk, 1)}, filepath.Join(dir, "ga.cow"), filepath.Join(dir, "ga.cow.bitmap"), groupChunk)
	require.NoError(t, err)
	defer a.Close()
	_, err = a.ReadAt(got, 6*groupChunk)
	require.NoError(t, err)
	require.Equal(t, w, got, "a write through the alias must survive the group's shutdown")
}

// When the device cannot be published, Serve fails with the base closed exactly once.
func TestServeFailedPublishClosesOnce(t *testing.T) {
	requireUblk(t)
	dir := t.TempDir()
	notADir := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(notADir, nil, 0600))
	base := &countingBase{bytesSource: bytesSource{data: make([]byte, 1<<20)}}
	_, err := Serve(context.Background(), &Options{ID: "pub", Base: base, COWFile: filepath.Join(dir, "pub.cow"), DevDir: filepath.Join(notADir, "dev"), RunDir: filepath.Join(dir, "run")})
	require.Error(t, err)
	require.Equal(t, 1, base.closes)
}
