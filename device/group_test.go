package device

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
	return openTestStoreOver(t, dir, id, &bytesSource{data: data})
}

func openTestStoreOver(t *testing.T, dir, id string, base source.Source) *cow.Store {
	t.Helper()
	s, err := cow.Open(base, filepath.Join(dir, id+".cow"), filepath.Join(dir, id+".bitmap"), groupChunk)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func readAt(t *testing.T, r io.ReaderAt, off int64, n int) []byte {
	t.Helper()
	p := make([]byte, n)
	_, err := r.ReadAt(p, off)
	require.NoError(t, err)
	return p
}

// lookupOf resolves device ids to their stores, as ServeGroup does for the Binders.
func lookupOf(stores map[string]*cow.Store) source.Lookup {
	return func(id string) (io.ReaderAt, bool) {
		s, ok := stores[id]
		if !ok {
			return nil, false
		}
		return s, true
	}
}

// siblingSource is a base that implements Binder: a live view of the sibling device id, read
// through its COW store, so it sees what the guest wrote there.
type siblingSource struct {
	id     string
	size   int64
	lookup source.Lookup
}

func (s *siblingSource) Bind(lookup source.Lookup) { s.lookup = lookup }

// Durable forwards to the sibling's store (source.Durability), so a store over this view
// relies on what the sibling made durable only.
func (s *siblingSource) Durable(off, length int64) bool {
	if s.lookup == nil {
		return false
	}
	a, ok := s.lookup(s.id)
	if !ok {
		return false
	}
	d, ok := a.(source.Durability)
	return ok && d.Durable(off, length)
}
func (s *siblingSource) ReadAt(p []byte, off int64) (int, error) {
	if s.lookup == nil {
		return 0, io.ErrUnexpectedEOF
	}
	a, ok := s.lookup(s.id)
	if !ok {
		return 0, io.ErrUnexpectedEOF
	}
	return a.ReadAt(p, off)
}
func (s *siblingSource) Size() int64  { return s.size }
func (s *siblingSource) Close() error { return nil }

func TestBinderSeesSiblingWrites(t *testing.T) {
	dir := t.TempDir()
	a := openTestStore(t, dir, "a", seeded(8*groupChunk, 1))
	view := &siblingSource{id: "a", size: 8 * groupChunk}
	b := openTestStoreOver(t, dir, "b", view)
	view.Bind(lookupOf(map[string]*cow.Store{"a": a, "b": b}))
	_, err := a.WriteAt([]byte{9, 9, 9}, 100)
	require.NoError(t, err)
	require.Equal(t, []byte{9, 9, 9}, readAt(t, b, 100, 3), "b's base reads a through its store, overlay included")
}

// The mirror case: b's base is a live view of a and both skip identical writes (nopwrite). The guest
// writes the same bytes to both plexes, then a resync copy that was read from a before the
// guest's write lands on b alone. a must keep the guest's bytes: b's writes go to b's own
// store. (Alias ranges, which forwarded b's writes into a's store, let that stale copy
// overwrite a, the only copy of the data; they were removed for it.)
func TestMirrorPlexNeverClobbersItsSibling(t *testing.T) {
	dir := t.TempDir()
	a := openTestStore(t, dir, "a", seeded(8*groupChunk, 1))
	a.EnableReclaim()
	a.TrackDurability()
	view := &siblingSource{id: "a", size: 8 * groupChunk}
	b := openTestStoreOver(t, dir, "b", view)
	b.EnableReclaim()
	a.SetNopWrite(true)
	b.SetNopWrite(true)
	view.Bind(lookupOf(map[string]*cow.Store{"a": a, "b": b}))
	off := int64(3*groupChunk + 100)
	x, y := bytes.Repeat([]byte{0xAA}, 200), bytes.Repeat([]byte{0xBB}, 200)
	// a mirrored write whose first half a made durable before b's half lands; b relies on
	// a's content only once it is durable (see TestNopWriteNeverReliesOnAnUnflushedSibling)
	writeBoth := func(p []byte) {
		_, err := a.WriteAt(p, off)
		require.NoError(t, err)
		require.NoError(t, a.Flush())
		_, err = b.WriteAt(p, off)
		require.NoError(t, err)
	}
	writeBoth(x)
	writeBoth(y)
	require.EqualValues(t, 1, a.Written())
	require.EqualValues(t, 0, b.Written(), "b's half of a mirrored write equals what b reads through a: skipped")
	require.Equal(t, y, readAt(t, b, off, 200))
	// the resync read x from a before the guest wrote y, and writes it to b now
	_, err := b.WriteAt(x, off)
	require.NoError(t, err)
	require.Equal(t, y, readAt(t, a, off, 200), "a keeps the guest's bytes")
	require.Equal(t, x, readAt(t, b, off, 200), "b is stale, as a real disk would be")
	require.EqualValues(t, 1, b.Written())
	// Windows re-copies the dirty region
	_, err = b.WriteAt(y, off)
	require.NoError(t, err)
	require.Equal(t, y, readAt(t, b, off, 200))
	// a resync of a range b never wrote stores nothing
	_, err = b.WriteAt(readAt(t, a, 4*groupChunk, 4*groupChunk), 4*groupChunk)
	require.NoError(t, err)
	require.EqualValues(t, 1, b.Written())
	// and such a range keeps following a
	_, err = a.WriteAt([]byte{7, 7, 7}, 5*groupChunk)
	require.NoError(t, err)
	require.Equal(t, []byte{7, 7, 7}, readAt(t, b, 5*groupChunk, 3), "an unwritten range of b follows a")
	require.NoError(t, a.Flush())
	// a sweep finds the chunk the re-copy wrote identical to what b reads through a again and
	// drops it, so b follows a there once more and the next mirrored write is a nopwrite again
	require.Equal(t, 1, b.Reclaim(context.Background(), 256), "one chunk was written since the last sweep")
	require.EqualValues(t, 0, b.Written(), "the re-copied chunk equals plex 0 again: reclaimed")
	require.Equal(t, y, readAt(t, b, off, 200))
	_, err = a.WriteAt([]byte{8, 8, 8}, off)
	require.NoError(t, err)
	require.Equal(t, []byte{8, 8, 8}, readAt(t, b, off, 3), "the reclaimed chunk follows a again")
}

// Bases are usually stitched from parts (a config always yields a Concat): every Binder in
// the tree gets the lookup, not only a Binder at the top.
func TestBindReachesNestedSources(t *testing.T) {
	nested := &siblingSource{id: "a", size: groupChunk}
	concat, err := source.NewConcat([]*source.Segment{{Offset: 0, Source: nested}}, 0)
	require.NoError(t, err)
	top := &siblingSource{id: "a", size: groupChunk}
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
	_, err = ServeGroup(ctx, []*Options{{ID: "cy", Base: base, COWFile: filepath.Join(dir, "cy.cow"), RunDir: dir, DevDir: dir}})
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

// Through the kernel: gb's base is a live view of ga. gb is listed first, so it comes up
// before the device it reads, and a write buffered in gb's page cache is only written out
// when gb stops; the group must still have ga's store open then. The write lands in gb's
// store and never in ga's.
func TestServeGroupThroughKernel(t *testing.T) {
	requireUblk(t)
	dir := t.TempDir()
	opt := func(id string, base source.Source) *Options {
		return &Options{ID: id, Base: base, COWFile: filepath.Join(dir, id+".cow"), ChunkSize: groupChunk,
			DevDir: filepath.Join(dir, "dev"), RunDir: filepath.Join(dir, "run"), NopWrite: true}
	}
	g, err := ServeGroup(context.Background(), []*Options{
		opt("gb", &siblingSource{id: "ga", size: 8 * groupChunk}),
		opt("ga", &bytesSource{data: seeded(8*groupChunk, 1)}),
	})
	require.NoError(t, err)
	fb, err := os.OpenFile(g.Devices["gb"].BlockPath, os.O_RDWR, 0)
	require.NoError(t, err)
	require.Equal(t, seeded(8*groupChunk, 1)[5*groupChunk:6*groupChunk], readAt(t, fb, 5*groupChunk, groupChunk), "gb shows ga's bytes")
	w := bytes.Repeat([]byte{0xC3}, groupChunk)
	_, err = fb.WriteAt(w, 2*groupChunk) // buffered, never flushed by us
	require.NoError(t, err)
	require.NoError(t, fb.Close())
	require.NoError(t, g.Close())
	b, err := cow.Open(&bytesSource{data: seeded(8*groupChunk, 1)}, filepath.Join(dir, "gb.cow"), filepath.Join(dir, "gb.cow.bitmap"), groupChunk)
	require.NoError(t, err)
	defer b.Close()
	require.Equal(t, w, readAt(t, b, 2*groupChunk, groupChunk), "the write must survive the group's shutdown in gb's store")
	require.EqualValues(t, 1, b.Written())
	a, err := cow.Open(&bytesSource{data: seeded(8*groupChunk, 1)}, filepath.Join(dir, "ga.cow"), filepath.Join(dir, "ga.cow.bitmap"), groupChunk)
	require.NoError(t, err)
	defer a.Close()
	require.EqualValues(t, 0, a.Written(), "nothing written through gb reaches ga")
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

const groupHelperEnv = "BLKMAP_DEVICE_GROUP_HELPER" // the test directory

// groupFailOptions is a mirror: fb's base is a live view of fa, with its own overlay.
func groupFailOptions(dir string) []*Options {
	opt := func(id string, base source.Source) *Options {
		return &Options{ID: id, Base: base, COWFile: filepath.Join(dir, id+".cow"), ChunkSize: groupChunk,
			DevDir: filepath.Join(dir, "dev"), RunDir: filepath.Join(dir, "run"), Recovery: true}
	}
	return []*Options{
		opt("fb", &siblingSource{id: "fa", size: 8 * groupChunk}),
		opt("fa", &bytesSource{data: seeded(8*groupChunk, 1)}),
	}
}

// TestHelperServeGroupFailing serves the mirror until SIGUSR1, then fails fa's store and does
// what an owner must: wait for the group to report it, abandon it and exit for a successor.
func TestHelperServeGroupFailing(t *testing.T) {
	dir := os.Getenv(groupHelperEnv)
	if dir == "" {
		t.Skip("helper process only")
	}
	usr1 := make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1)
	g, err := ServeGroup(context.Background(), groupFailOptions(dir))
	if err != nil {
		fmt.Println("ERR", err)
		os.Exit(1)
	}
	fmt.Println("DEV", g.Devices["fb"].BlockPath, g.Devices["fa"].BlockPath)
	<-usr1
	g.Devices["fa"].store.Fail(errors.New("injected"))
	select {
	case <-g.Done():
	case <-time.After(5 * time.Second):
		fmt.Println("ERR the group did not report the failure")
		os.Exit(1)
	}
	fmt.Println("FAILED", g.Err())
	if err := g.Abandon(); err != nil && !errors.Is(err, cow.ErrCOWFailed) {
		fmt.Println("ERR abandon:", err)
		os.Exit(1)
	}
	os.Exit(ExitDetached)
}

func TestGroupStoreFailureHandsOffToSuccessor(t *testing.T) {
	requireUblk(t)
	if !recoverySupported() {
		t.Skip("no ublk user recovery")
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run", "TestHelperServeGroupFailing$")
	cmd.Env = append(os.Environ(), groupHelperEnv+"="+dir)
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	r := bufio.NewReader(out)
	var fbPath, faPath string
	_, err = fmt.Fscanf(r, "DEV %s %s\n", &fbPath, &faPath)
	require.NoError(t, err)
	fa, err := os.OpenFile(faPath, os.O_RDWR|syscall.O_DIRECT, 0)
	require.NoError(t, err)
	defer fa.Close()
	// One write flushed, one acknowledged but not flushed when the store fails
	flushed := writeUnflushed(t, fa, 0, 'f')
	require.NoError(t, fa.Sync())
	writeUnflushed(t, fa, groupChunk, 'u')
	require.NoError(t, cmd.Process.Signal(syscall.SIGUSR1))
	rest, _ := io.ReadAll(r)
	err = cmd.Wait()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit, "helper output: %s", rest)
	require.Equal(t, ExitDetached, exit.ExitCode(), "helper output: %s", rest)
	assert.Contains(t, string(rest), "FAILED fa: cow file failed")
	// The successor re-attaches both devices and serves the last flushed state
	g, err := ServeGroup(context.Background(), groupFailOptions(dir))
	require.NoError(t, err)
	assert.Equal(t, faPath, g.Devices["fa"].BlockPath)
	assert.Equal(t, flushed, readBlock(t, fa, 0))
	assert.Equal(t, seeded(8*groupChunk, 1)[groupChunk:groupChunk+4096], readBlock(t, fa, groupChunk), "the unflushed write may be lost with the failed store")
	fb, err := os.OpenFile(fbPath, os.O_RDONLY|syscall.O_DIRECT, 0)
	require.NoError(t, err)
	defer fb.Close()
	assert.Equal(t, flushed, readBlock(t, fb, 0), "fb's view still shows fa")
	// Deleting a kernel device waits for its openers
	require.NoError(t, fb.Close())
	require.NoError(t, fa.Close())
	require.NoError(t, g.Close())
}
