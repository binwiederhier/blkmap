package device

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"heckel.io/blkmap/source"
)

func TestRecorderWritesAccessesInOrder(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "rec")
	start := time.Now()
	r, err := newRecorder("d", path, &Record{File: path}, start, 1024)
	require.NoError(t, err)
	r.add(false, 0, 4096, start)
	r.add(true, 8192, 512, start.Add(3*time.Millisecond))
	r.add(false, 1<<20, 65536, start.Add(41*time.Millisecond))
	r.finish("stopped")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	accesses, err := source.ParseRecording(strings.NewReader(string(data)))
	require.NoError(t, err)
	assert.Equal(t, []source.Access{
		{Millis: 0, Offset: 0, Length: 4096},
		{Millis: 3, Write: true, Offset: 8192, Length: 512},
		{Millis: 41, Offset: 1 << 20, Length: 65536},
	}, accesses)
	assert.Contains(t, string(data), "# blkmap recording of d")
	assert.Contains(t, string(data), "# stopped: 3 requests, 0 dropped")
	st := r.status()
	assert.Equal(t, int64(3), st.Requests)
	assert.False(t, st.Active)
}

func TestRecorderRefusesExistingFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "rec")
	require.NoError(t, os.WriteFile(path, []byte("0 R 0 4096\n"), 0600))
	_, err := newRecorder("d", path, &Record{File: path}, time.Now(), 1024)
	assert.ErrorIs(t, err, os.ErrExist, "a finished recording is never overwritten, so restarts do not clobber it")
}

func TestRecorderStopsAfterMaxDuration(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "rec")
	start := time.Now()
	r, err := newRecorder("d", path, &Record{File: path, MaxDuration: 10 * time.Millisecond}, start, 1024)
	require.NoError(t, err)
	r.add(false, 0, 4096, start.Add(5*time.Millisecond))
	r.add(false, 4096, 4096, start.Add(20*time.Millisecond)) // past the limit
	assert.True(t, r.flush(start.Add(20*time.Millisecond)), "the duration limit ends the recording")
	r.finish("max-duration reached")
	data, _ := os.ReadFile(path)
	accesses, err := source.ParseRecording(strings.NewReader(string(data)))
	require.NoError(t, err)
	assert.Len(t, accesses, 1)
}

func TestRecorderStopsAtMaxSize(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "rec")
	start := time.Now()
	r, err := newRecorder("d", path, &Record{File: path, MaxSize: 200}, start, 1024)
	require.NoError(t, err)
	for i := 0; i < 100; i++ {
		r.add(false, int64(i)*4096, 4096, start)
	}
	assert.True(t, r.flush(start), "the size limit ends the recording")
	r.finish("max-size reached")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Less(t, info.Size(), int64(400), "only whole lines up to the limit are written")
	data, _ := os.ReadFile(path)
	_, err = source.ParseRecording(strings.NewReader(string(data)))
	require.NoError(t, err, "the file stays parseable")
}

func TestRecorderDropsWhenFull(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "rec")
	start := time.Now()
	r, err := newRecorder("d", path, &Record{File: path}, start, 4)
	require.NoError(t, err)
	for i := 0; i < 10; i++ {
		r.add(false, int64(i)*4096, 4096, start)
	}
	r.finish("stopped")
	st := r.status()
	assert.Equal(t, int64(4), st.Requests)
	assert.Equal(t, int64(6), st.Dropped)
	data, _ := os.ReadFile(path)
	assert.Contains(t, string(data), "4 requests, 6 dropped")
}

func TestRecorderAddDoesNotAllocate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rec")
	start := time.Now()
	r, err := newRecorder("d", path, &Record{File: path}, start, 1<<16)
	require.NoError(t, err)
	defer r.finish("stopped")
	assert.Zero(t, testing.AllocsPerRun(1000, func() { r.add(false, 0, 4096, start) }))
}

func TestRecorderRunFlushesAndEndsWithContext(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "rec")
	b := &backend{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r, err := newRecorder("d", path, &Record{File: path}, time.Now(), 1024)
	require.NoError(t, err)
	b.rec.Store(r)
	go func() {
		defer close(done)
		r.run(ctx, b)
	}()
	b.record(false, 0, 4096, time.Now())
	cancel()
	<-done
	assert.Nil(t, b.rec.Load(), "the backend stops recording once the recorder ends")
	data, _ := os.ReadFile(path)
	assert.Contains(t, string(data), " R 0 4096\n")
	assert.Contains(t, string(data), "# stopped: 1 requests")
}
