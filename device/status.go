package device

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"heckel.io/blkmap/cow"
	"heckel.io/blkmap/source"
)

const (
	// statusSocketExt names a device's status socket in RunDir (<id>.sock).
	statusSocketExt  = ".sock"
	statusSocketMode = 0600
	statusPath       = "/status"
	metricsPath      = "/metrics"
	statusHost       = "http://blkmap"
	statusTimeout    = 5 * time.Second
	metricsType      = "text/plain; version=0.0.4"
)

var (
	// latencyBucketNanos are the request latency histogram bounds (100 us to 2.5 s).
	latencyBucketNanos = [...]int64{1e5, 2.5e5, 5e5, 1e6, 2.5e6, 5e6, 1e7, 2.5e7, 5e7, 1e8, 2.5e8, 5e8, 1e9, 2.5e9}
)

// Status is a snapshot of a running device, served on its status socket.
type Status struct {
	ID             string             `json:"id"`
	Path           string             `json:"path"`
	BlockPath      string             `json:"block_path"`
	PID            int                `json:"pid"`
	Started        time.Time          `json:"started"`
	Recovered      bool               `json:"recovered"` // this server re-attached to a running device
	ReadOnly       bool               `json:"read_only"`
	COWFile        string             `json:"cow_file,omitempty"` // empty: no overlay (read-only, no hydration)
	Size           int64              `json:"size"`
	ChunkSize      int64              `json:"chunk_size"`
	Chunks         int64              `json:"chunks"`
	Written        int64              `json:"written"`
	Dirty          bool               `json:"dirty"`
	Queues         int                `json:"queues"`
	ParallelQueues int                `json:"parallel_queues"`
	IO             IOStats            `json:"io"`
	Source         cow.SourceStats    `json:"source"`
	Cache          *source.CacheStats `json:"cache,omitempty"`
	Hydration      *Progress          `json:"hydration,omitempty"`
	Reclaim        *cow.ReclaimStats  `json:"reclaim,omitempty"` // nil unless Options.Reclaim
	Recording      *RecordStatus      `json:"recording,omitempty"`
}

// IOStats counts guest requests.
type IOStats struct {
	Reads        int64     `json:"reads"`
	Writes       int64     `json:"writes"`
	Flushes      int64     `json:"flushes"`
	Discards     int64     `json:"discards"`
	WriteZeroes  int64     `json:"write_zeroes"`
	ReadBytes    int64     `json:"read_bytes"`
	WriteBytes   int64     `json:"write_bytes"`
	Errors       int64     `json:"errors"`
	Inflight     int64     `json:"inflight"`
	ReadLatency  Histogram `json:"read_latency"`
	WriteLatency Histogram `json:"write_latency"`
}

// Histogram is a latency distribution over latencyBuckets; Counts has one more entry than
// the bounds, for everything above the last.
type Histogram struct {
	Counts []int64 `json:"counts"`
	Count  int64   `json:"count"`
	Sum    float64 `json:"sum"` // seconds
}

// metric is one Prometheus metric family and how to read its samples from a Status.
type metric struct {
	name, kind, help string
	samples          func(st *Status, emit func(labels string, value float64))
}

// metrics lists every exported family; the device label is added to each sample.
var metrics = []metric{
	{"blkmap_up", "gauge", "The device is served.", func(st *Status, emit func(string, float64)) { emit("", 1) }},
	{"blkmap_size_bytes", "gauge", "Device size.", func(st *Status, emit func(string, float64)) { emit("", float64(st.Size)) }},
	{"blkmap_chunks", "gauge", "COW chunks in the device.", func(st *Status, emit func(string, float64)) { emit("", float64(st.Chunks)) }},
	{"blkmap_chunks_written", "gauge", "COW chunks in the COW file (written or hydrated).", func(st *Status, emit func(string, float64)) { emit("", float64(st.Written)) }},
	{"blkmap_dirty", "gauge", "Completed writes not yet flushed.", func(st *Status, emit func(string, float64)) { emit("", boolValue(st.Dirty)) }},
	{"blkmap_recovered", "gauge", "This server re-attached to a running device after a crash or handoff.", func(st *Status, emit func(string, float64)) { emit("", boolValue(st.Recovered)) }},
	{"blkmap_start_time_seconds", "gauge", "When this server started.", func(st *Status, emit func(string, float64)) {
		emit("", float64(st.Started.UnixNano())/1e9)
	}},
	{"blkmap_queues", "gauge", "ublk queues.", func(st *Status, emit func(string, float64)) { emit("", float64(st.Queues)) }},
	{"blkmap_queues_parallel", "gauge", "Queues handing reads to workers (a slow source).", func(st *Status, emit func(string, float64)) {
		emit("", float64(st.ParallelQueues))
	}},
	{"blkmap_requests_total", "counter", "Guest requests.", func(st *Status, emit func(string, float64)) {
		emit(`op="read"`, float64(st.IO.Reads))
		emit(`op="write"`, float64(st.IO.Writes))
		emit(`op="flush"`, float64(st.IO.Flushes))
		emit(`op="discard"`, float64(st.IO.Discards))
		emit(`op="write_zeroes"`, float64(st.IO.WriteZeroes))
	}},
	{"blkmap_request_bytes_total", "counter", "Guest bytes transferred.", func(st *Status, emit func(string, float64)) {
		emit(`op="read"`, float64(st.IO.ReadBytes))
		emit(`op="write"`, float64(st.IO.WriteBytes))
	}},
	{"blkmap_request_errors_total", "counter", "Guest requests that failed (EIO).", func(st *Status, emit func(string, float64)) { emit("", float64(st.IO.Errors)) }},
	{"blkmap_requests_inflight", "gauge", "Guest requests being served.", func(st *Status, emit func(string, float64)) { emit("", float64(st.IO.Inflight)) }},
	{"blkmap_request_duration_seconds", "histogram", "Guest request latency.", func(st *Status, emit func(string, float64)) {
		histogram(`op="read"`, st.IO.ReadLatency, emit)
		histogram(`op="write"`, st.IO.WriteLatency, emit)
	}},
	{"blkmap_source_reads_total", "counter", "Reads from the base source.", func(st *Status, emit func(string, float64)) { emit("", float64(st.Source.Reads)) }},
	{"blkmap_source_read_bytes_total", "counter", "Bytes read from the base source.", func(st *Status, emit func(string, float64)) { emit("", float64(st.Source.Bytes)) }},
	{"blkmap_source_read_errors_total", "counter", "Failed reads from the base source.", func(st *Status, emit func(string, float64)) {
		emit("", float64(st.Source.Errors))
	}},
	{"blkmap_source_read_seconds_total", "counter", "Time spent reading the base source.", func(st *Status, emit func(string, float64)) {
		emit("", st.Source.Duration.Seconds())
	}},
	{"blkmap_cache_reads_total", "counter", "Reads of cache tiers by outcome.", func(st *Status, emit func(string, float64)) {
		if st.Cache != nil {
			emit(`result="hit"`, float64(st.Cache.Hits))
			emit(`result="miss"`, float64(st.Cache.Misses))
			emit(`result="failure"`, float64(st.Cache.Failures))
		}
	}},
	{"blkmap_hydration_chunks", "gauge", "Chunks in the COW file, as hydration counts them.", func(st *Status, emit func(string, float64)) {
		if st.Hydration != nil {
			emit("", float64(st.Hydration.Hydrated))
		}
	}},
	{"blkmap_hydration_copied_bytes_total", "counter", "Bytes hydration copied.", func(st *Status, emit func(string, float64)) {
		if st.Hydration != nil {
			emit("", float64(st.Hydration.Copied))
		}
	}},
	{"blkmap_source_demand_reads_total", "counter", "Reads from the base source that a guest request needed (chunks not in the COW file).", func(st *Status, emit func(string, float64)) {
		emit("", float64(st.Source.DemandReads))
	}},
	{"blkmap_source_demand_read_bytes_total", "counter", "Bytes a guest request needed from the base source.", func(st *Status, emit func(string, float64)) {
		emit("", float64(st.Source.DemandBytes))
	}},
	{"blkmap_hydration_late_chunks_total", "counter", "Listed chunks the guest read from the source before hydration copied them.", func(st *Status, emit func(string, float64)) {
		if st.Hydration != nil {
			emit("", float64(st.Hydration.Late))
		}
	}},
	{"blkmap_hydration_errors_total", "counter", "Failed hydration reads (retried in later passes).", func(st *Status, emit func(string, float64)) {
		if st.Hydration != nil {
			emit("", float64(st.Hydration.Errors))
		}
	}},
	{"blkmap_hydration_lead_seconds", "gauge", "How far hydration is ahead of (> 0) or behind (< 0) a timed prefetch list.", func(st *Status, emit func(string, float64)) {
		if st.Hydration != nil && st.Hydration.Schedule != nil {
			emit("", st.Hydration.Schedule.Lead.Seconds())
		}
	}},
	{"blkmap_hydration_behind_chunks", "gauge", "Listed chunks the recording had read by now that hydration has not copied yet.", func(st *Status, emit func(string, float64)) {
		if st.Hydration != nil && st.Hydration.Schedule != nil {
			emit("", float64(st.Hydration.Schedule.Behind))
		}
	}},
	{"blkmap_hydration_done", "gauge", "Hydration has ended.", func(st *Status, emit func(string, float64)) {
		if st.Hydration != nil {
			emit("", boolValue(st.Hydration.Done))
		}
	}},
	{"blkmap_reclaim_examined_total", "counter", "Chunks the sweeper compared with their base.", func(st *Status, emit func(string, float64)) {
		if st.Reclaim != nil {
			emit("", float64(st.Reclaim.Examined))
		}
	}},
	{"blkmap_reclaim_chunks_total", "counter", "Stored chunks dropped from the COW file because they equalled their base again.", func(st *Status, emit func(string, float64)) {
		if st.Reclaim != nil {
			emit("", float64(st.Reclaim.Chunks))
		}
	}},
	{"blkmap_reclaim_bytes_total", "counter", "Bytes those chunks held.", func(st *Status, emit func(string, float64)) {
		if st.Reclaim != nil {
			emit("", float64(st.Reclaim.Bytes))
		}
	}},
	{"blkmap_reclaim_pending_chunks", "gauge", "Chunks written since the sweeper last examined them.", func(st *Status, emit func(string, float64)) {
		if st.Reclaim != nil {
			emit("", float64(st.Reclaim.Pending))
		}
	}},
}

// WriteMetrics writes the statuses in the Prometheus text format, each family once.
func WriteMetrics(w io.Writer, statuses ...*Status) error {
	bw := bufio.NewWriter(w)
	for _, m := range metrics {
		fmt.Fprintf(bw, "# HELP %s %s\n# TYPE %s %s\n", m.name, m.help, m.name, m.kind)
		for _, st := range statuses {
			device := `device="` + escapeLabel(st.ID) + `"`
			m.samples(st, func(labels string, value float64) {
				name, all := m.name, device
				if strings.HasPrefix(labels, "_") { // histogram series: _bucket{...}, _sum, _count
					suffix, rest, _ := strings.Cut(labels, "|")
					name, labels = name+suffix, rest
				}
				if labels != "" {
					all += "," + labels
				}
				fmt.Fprintf(bw, "%s{%s} %s\n", name, all, strconv.FormatFloat(value, 'g', -1, 64))
			})
		}
	}
	return bw.Flush()
}

// histogram emits a histogram's series (cumulative buckets, sum, count) for labels.
func histogram(labels string, h Histogram, emit func(string, float64)) {
	var cumulative int64
	for i, n := range h.Counts {
		cumulative += n
		le := "+Inf"
		if i < len(latencyBucketNanos) {
			le = strconv.FormatFloat(float64(latencyBucketNanos[i])/1e9, 'g', -1, 64)
		}
		emit("_bucket|"+labels+`,le="`+le+`"`, float64(cumulative))
	}
	emit("_sum|"+labels, h.Sum)
	emit("_count|"+labels, float64(h.Count))
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// ListenStatus serves status on a unix socket at path (root only) until closed: JSON on
// /status, Prometheus text on /metrics. A stale socket of a dead server is replaced.
func ListenStatus(path string, status func() *Status) (io.Closer, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, statusSocketMode); err != nil {
		l.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc(statusPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status())
	})
	mux.HandleFunc(metricsPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", metricsType)
		WriteMetrics(w, status())
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: statusTimeout}
	go srv.Serve(l)
	return srv, nil
}

// QueryStatus asks the server of device id for its status.
func QueryStatus(runDir, id string) (*Status, error) {
	path := filepath.Join(runDir, id+statusSocketExt)
	// One query per transport: without DisableKeepAlives the socket would idle in a pool
	// nobody closes, leaking a socket pair per query in a process that polls
	client := &http.Client{Timeout: statusTimeout, Transport: &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}}}
	resp, err := client.Get(statusHost + statusPath)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", id, err)
	}
	defer resp.Body.Close()
	st := &Status{}
	if err := json.NewDecoder(resp.Body).Decode(st); err != nil {
		return nil, fmt.Errorf("%s: %w", id, err)
	}
	return st, nil
}

// QueryAll returns the status of every device served on this machine. Sockets nobody
// listens on (a crashed server's) are skipped.
func QueryAll(runDir string) ([]*Status, error) {
	paths, err := filepath.Glob(filepath.Join(runDir, "*"+statusSocketExt))
	if err != nil {
		return nil, err
	}
	var statuses []*Status
	for _, path := range paths {
		st, err := QueryStatus(runDir, strings.TrimSuffix(filepath.Base(path), statusSocketExt))
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		statuses = append(statuses, st)
	}
	return statuses, nil
}
