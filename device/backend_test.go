package device

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failing is a Backend whose reads fail.
type failing struct {
	size int64
}

func (f *failing) ReadAt(p []byte, off int64) (int, error) {
	return 0, errors.New("source gone")
}

func (f *failing) WriteAt(p []byte, off int64) (int, error) {
	return len(p), nil
}

func (f *failing) Size() int64 {
	return f.size
}

func (f *failing) Flush() error {
	return nil
}

func (f *failing) Close() error {
	return nil
}

func TestBackendLogsErrors(t *testing.T) {
	var out bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(prev) })
	b := &backend{Backend: &failing{size: 1 << 20}, id: "d"}
	_, err := b.ReadAt(make([]byte, 4096), 8192)
	require.Error(t, err)
	assert.Contains(t, out.String(), "d: read at offset 8192 (4096 bytes) failed: source gone")
	_, err = b.WriteAt(make([]byte, 512), 0)
	require.NoError(t, err)
	require.NoError(t, b.Flush())
	assert.Equal(t, 1, strings.Count(out.String(), "\n"))
	// Flooding is capped
	for i := 0; i < 2*maxLoggedErrors; i++ {
		b.ReadAt(make([]byte, 512), 0)
	}
	assert.Equal(t, maxLoggedErrors, strings.Count(out.String(), "\n"))
	assert.Contains(t, out.String(), "further I/O errors not logged")
}
