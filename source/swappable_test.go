package source

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slow is a mem source whose reads block until released, to observe in-flight behaviour.
type slowSource struct {
	mem
	release chan struct{}
}

func (s *slowSource) ReadAt(p []byte, off int64) (int, error) {
	<-s.release
	return s.mem.ReadAt(p, off)
}

func TestSwappable(t *testing.T) {
	t.Parallel()
	a := &slowSource{mem: mem{data: filled(1024, 'a').data}, release: make(chan struct{})}
	b := filled(1024, 'b')
	s := NewSwappable(a)
	assert.Equal(t, int64(1024), s.Size())
	// A read in flight on a must finish before Swap returns
	var wg sync.WaitGroup
	wg.Add(1)
	got := make([]byte, 4)
	go func() {
		defer wg.Done()
		_, err := s.ReadAt(got, 0)
		assert.NoError(t, err)
	}()
	time.Sleep(20 * time.Millisecond)
	swapped := make(chan Source, 1)
	go func() {
		old, err := s.Swap(b)
		assert.NoError(t, err)
		swapped <- old
	}()
	select {
	case <-swapped:
		t.Fatal("Swap returned while a read was in flight on the old source")
	case <-time.After(50 * time.Millisecond):
	}
	close(a.release)
	wg.Wait()
	old := <-swapped
	assert.Same(t, a, old)
	assert.Equal(t, []byte("aaaa"), got)
	_, err := s.ReadAt(got, 0)
	require.NoError(t, err)
	assert.Equal(t, []byte("bbbb"), got)
	// Size mismatch is refused
	_, err = s.Swap(filled(512, 'c'))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "size")
	require.NoError(t, s.Close())
	assert.True(t, b.closed)
}
