package device

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"heckel.io/blkmap/source"
	"heckel.io/blkmap/util"
)

const (
	// recordBuffer is how many requests the recorder holds between flushes (24 bytes each);
	// beyond that, requests are dropped and counted rather than slowing the guest down.
	recordBuffer = 1 << 16
	// recordFlushInterval is how often the recorder writes what it holds to the file.
	recordFlushInterval  = time.Second
	recordFileMode       = 0600
	recordReasonStopped  = "stopped"
	recordReasonDuration = "max-duration reached"
	recordReasonSize     = "max-size reached"
)

// Record configures a recording of guest I/O: every request, as "millis R|W offset length"
// lines, the raw material of a prefetch list (see source.CompactRecording). A recording
// file is never overwritten, so a restarted server does not clobber it; delete the file to
// record again.
type Record struct {
	File        string
	MaxDuration time.Duration // 0 = until the device stops
	MaxSize     int64         // file size limit; 0 = none
}

// RecordStatus is the state of a recording.
type RecordStatus struct {
	File     string `json:"file"`
	Requests int64  `json:"requests"` // requests written to the file
	Dropped  int64  `json:"dropped"`  // requests lost because the buffer was full
	Active   bool   `json:"active"`
}

// ioRecorder records guest requests. add, on the I/O path, appends to a fixed buffer under
// a mutex and never allocates; a background flush swaps the buffer and formats it to the
// file, so the guest never waits for the disk.
type ioRecorder struct {
	id          string
	path        string
	start       time.Time
	maxDuration time.Duration
	maxSize     int64
	f           *os.File
	size        int64  // bytes in the file (flush and finish only)
	line        []byte // format buffer (flush and finish only)
	full        bool   // the size limit was reached (flush and finish only)
	reason      string // why the recording ends, once it does (flush and finish only)
	spare       []recordEntry
	requests    atomic.Int64
	dropped     atomic.Int64
	buf         []recordEntry
	active      bool
	mu          sync.Mutex // Protects buf and active
}

// recordEntry is one request as add stores it.
type recordEntry struct {
	nanos  int64
	write  bool
	offset int64
	length int64
}

// newRecorder creates the recording file, which must not exist, and writes its header.
func newRecorder(id, path string, o *Record, start time.Time, capacity int) (*ioRecorder, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, recordFileMode)
	if err != nil {
		return nil, err
	}
	r := &ioRecorder{id: id, path: path, start: start, maxDuration: o.MaxDuration, maxSize: o.MaxSize, f: f,
		buf: make([]recordEntry, 0, capacity), spare: make([]recordEntry, 0, capacity), active: true}
	header := fmt.Sprintf("# blkmap recording of %s, started %s; millis R|W offset length\n", id, start.UTC().Format(time.RFC3339))
	n, err := f.WriteString(header)
	r.size = int64(n)
	if err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	return r, nil
}

// add records one request; now is when it started. Past max-duration, when the buffer is
// full, or after the recording ended, it records nothing.
func (r *ioRecorder) add(write bool, offset, length int64, now time.Time) {
	elapsed := now.Sub(r.start)
	if r.maxDuration > 0 && elapsed > r.maxDuration {
		return
	}
	r.mu.Lock()
	if !r.active {
		r.mu.Unlock()
		return
	}
	if len(r.buf) == cap(r.buf) {
		r.mu.Unlock()
		r.dropped.Add(1)
		return
	}
	r.buf = append(r.buf, recordEntry{nanos: int64(elapsed), write: write, offset: offset, length: length})
	r.mu.Unlock()
}

// flush writes what add buffered to the file and reports whether the recording is over (a
// limit was reached or the file cannot be written).
func (r *ioRecorder) flush(now time.Time) bool {
	r.mu.Lock()
	entries := r.buf
	r.buf, r.spare = r.spare[:0], entries
	r.mu.Unlock()
	r.line = r.line[:0]
	for _, e := range entries {
		if r.full {
			break
		}
		before := len(r.line)
		r.line = source.AppendAccess(r.line, source.Access{Millis: e.nanos / int64(time.Millisecond), Write: e.write, Offset: e.offset, Length: e.length})
		if r.maxSize > 0 && r.size+int64(len(r.line)) > r.maxSize {
			r.line = r.line[:before]
			r.full = true
			r.reason = recordReasonSize
			break
		}
		r.requests.Add(1)
	}
	if len(r.line) > 0 {
		n, err := r.f.Write(r.line)
		r.size += int64(n)
		if err != nil {
			log.Printf("%s: recording to %s failed: %s", r.id, r.path, err.Error())
			r.full = true
			r.reason = err.Error()
		}
	}
	if r.reason == "" && r.maxDuration > 0 && now.Sub(r.start) >= r.maxDuration {
		r.reason = recordReasonDuration
	}
	return r.reason != ""
}

// run flushes every recordFlushInterval until a limit is reached or ctx ends, then detaches
// from b (whose I/O path is back to a nil check) and finishes the file.
func (r *ioRecorder) run(ctx context.Context, b *backend) {
	ticker := time.NewTicker(recordFlushInterval)
	defer ticker.Stop()
	for done := false; !done; {
		select {
		case <-ctx.Done():
			done = true
		case now := <-ticker.C:
			done = r.flush(now)
		}
	}
	b.rec.CompareAndSwap(r, nil)
	r.finish(recordReasonStopped)
}

// finish writes what is left and a closing comment, closes the file and frees the buffers.
// reason applies unless a limit already ended the recording. Idempotent.
func (r *ioRecorder) finish(reason string) {
	r.mu.Lock()
	if !r.active {
		r.mu.Unlock()
		return
	}
	r.active = false
	r.mu.Unlock()
	r.flush(time.Now())
	if r.reason != "" {
		reason = r.reason
	}
	fmt.Fprintf(r.f, "# %s: %d requests, %d dropped\n", reason, r.requests.Load(), r.dropped.Load())
	if err := r.f.Close(); err != nil {
		log.Printf("%s: closing recording %s: %s", r.id, r.path, err.Error())
	}
	r.buf, r.spare, r.line = nil, nil, nil
	log.Printf("%s: recording to %s ended (%s): %d requests, %d dropped, %s", r.id, r.path, reason, r.requests.Load(), r.dropped.Load(), util.FormatSize(r.size))
}

// abandon removes a recording that never got going (the device failed to start), so the
// next start can record.
func (r *ioRecorder) abandon() {
	r.mu.Lock()
	r.active = false
	r.mu.Unlock()
	r.f.Close()
	if err := os.Remove(r.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("%s: cannot remove recording %s: %s", r.id, r.path, err.Error())
	}
}

func (r *ioRecorder) status() RecordStatus {
	r.mu.Lock()
	active := r.active
	r.mu.Unlock()
	return RecordStatus{File: r.path, Requests: r.requests.Load(), Dropped: r.dropped.Load(), Active: active}
}

// record hands a request to the running recording, if any.
func (b *backend) record(write bool, offset, length int64, now time.Time) {
	if r := b.rec.Load(); r != nil {
		r.add(write, offset, length, now)
	}
}
