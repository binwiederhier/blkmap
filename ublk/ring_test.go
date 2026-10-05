package ublk

import (
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeEnter replaces io_uring_enter for one test: each call consumes what step returns of
// the submitted entries (advancing the ring's head like the kernel) and fails with its errno.
func fakeEnter(t *testing.T, head *uint32, step func(call int, pending uint32) (consumed uint32, errno syscall.Errno)) *int {
	t.Helper()
	calls := 0
	original := ringEnter
	t.Cleanup(func() { ringEnter = original })
	ringEnter = func(_, _, pending, _, _, _, _ uintptr) (uintptr, uintptr, syscall.Errno) {
		calls++
		consumed, errno := step(calls, uint32(pending))
		*head += consumed
		return uintptr(consumed), 0, errno
	}
	return &calls
}

// From the 2026-10-05 external review (finding 04): io_uring_enter may consume fewer entries
// than asked; flush ignored the count, and since the next flush computes what is pending from
// the tail it already published, the rest of the queue never reached the kernel.
func TestRingFlushSubmitsEverythingAfterShortSubmissions(t *testing.T) {
	var head, tail uint32
	calls := fakeEnter(t, &head, func(call int, pending uint32) (uint32, syscall.Errno) {
		return 1, 0 // one entry per call
	})
	r := &ring{local: 3, sqHead: &head, sqTail: &tail}
	require.NoError(t, r.flush())
	assert.Equal(t, tail, head, "every published entry is consumed")
	assert.Equal(t, 3, *calls)
}

// Interruptions, resource pressure and calls that make no progress are retried; only a
// submission that never progresses is an error.
func TestRingFlushRetriesWithoutProgress(t *testing.T) {
	var head, tail uint32
	fakeEnter(t, &head, func(call int, pending uint32) (uint32, syscall.Errno) {
		switch call {
		case 1:
			return 0, syscall.EINTR
		case 2:
			return 0, syscall.EAGAIN
		case 3:
			return 0, 0 // no progress, no error
		case 4:
			return 1, 0
		}
		return pending, 0
	})
	r := &ring{local: 4, sqHead: &head, sqTail: &tail}
	require.NoError(t, r.flush())
	assert.Equal(t, tail, head)

	var head2, tail2 uint32
	fakeEnter(t, &head2, func(int, uint32) (uint32, syscall.Errno) { return 0, syscall.EAGAIN })
	r = &ring{local: 2, sqHead: &head2, sqTail: &tail2}
	assert.Error(t, r.flush(), "a submission that never progresses is reported")
}
