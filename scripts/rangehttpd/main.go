// rangehttpd serves a directory over HTTP with Range support, for blkmap's e2e, stress,
// example and demo scripts; net/http serves Range requests concurrently and fast, which
// python's http.server does not. Options make it a stand-in for a remote origin:
//
//	rangehttpd DIR ADDR [-delay 20ms] [-rate 20M] [-disk 10ms,150M] [-holes-404] [-bytes-log FILE]
//
// -delay adds a fixed latency to every request (a network round trip; requests overlap),
// -rate caps the bandwidth of all responses together (bytes/s, binary suffixes), -disk makes
// the origin behave like one hard disk: requests are served one at a time, each costing a
// seek (waived when it continues where the previous one ended) plus its bytes at the given
// sequential rate, so random reads are slow and ordered prefetch is rewarded. -holes-404
// answers 404 to a Range request that touches a hole of a sparse file (a partial copy then
// behaves like a cache tier that lacks those blocks), and -bytes-log appends the body size
// of every response to a file.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"heckel.io/blkmap/util"
)

var (
	bytesMu sync.Mutex
)

// counting wraps a ResponseWriter to count body bytes and, with a limiter, pace them.
type counting struct {
	http.ResponseWriter
	n       int64
	limiter *limiter
}

func (c *counting) Write(p []byte) (int, error) {
	if c.limiter != nil {
		c.limiter.wait(len(p))
	}
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}

// disk serializes requests like a single spindle: a seek unless the request continues the
// previous one, then the bytes at the sequential rate.
type disk struct {
	seek    time.Duration
	rate    float64
	lastEnd int64
	mu      sync.Mutex // Protects lastEnd; held for the whole simulated service time
}

// serve holds the disk for the simulated time of reading [start, start+n).
func (d *disk) serve(start, n int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	cost := time.Duration(float64(n) / d.rate * float64(time.Second))
	if start != d.lastEnd {
		cost += d.seek
	}
	d.lastEnd = start + n
	time.Sleep(cost)
}

// limiter is a token bucket shared by all responses.
type limiter struct {
	rate   float64
	tokens float64
	last   time.Time
	mu     sync.Mutex // Protects tokens and last
}

func (l *limiter) wait(n int) {
	l.mu.Lock()
	now := time.Now()
	l.tokens = min(l.tokens+now.Sub(l.last).Seconds()*l.rate, l.rate/10) // burst: 100 ms worth
	l.last = now
	l.tokens -= float64(n)
	debt := l.tokens
	l.mu.Unlock()
	if debt < 0 {
		time.Sleep(time.Duration(-debt / l.rate * float64(time.Second)))
	}
}

func main() {
	fs := flag.NewFlagSet("rangehttpd", flag.ExitOnError)
	delay := fs.Duration("delay", 0, "delay added to every request")
	rate := fs.String("rate", "", "bandwidth cap for all responses together, bytes/s")
	diskSpec := fs.String("disk", "", "serve like one hard disk: SEEK,RATE (e.g. 10ms,150M)")
	holes404 := fs.Bool("holes-404", false, "answer 404 to Range requests that touch a hole of a sparse file")
	bytesLog := fs.String("bytes-log", "", "append the body size of every response to this file")
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: rangehttpd DIR ADDR [-delay 20ms] [-rate 20M] [-disk 10ms,150M] [-holes-404] [-bytes-log FILE]")
		os.Exit(2)
	}
	fs.Parse(os.Args[3:])
	var lim *limiter
	if *rate != "" {
		r, err := util.ParseSize(*rate)
		if err != nil || r <= 0 {
			log.Fatalf("invalid -rate %q", *rate)
		}
		lim = &limiter{rate: float64(r), last: time.Now()}
	}
	var hdd *disk
	if *diskSpec != "" {
		seekStr, rateStr, ok := strings.Cut(*diskSpec, ",")
		seek, err1 := time.ParseDuration(seekStr)
		r, err2 := util.ParseSize(rateStr)
		if !ok || err1 != nil || err2 != nil || r <= 0 {
			log.Fatalf("invalid -disk %q, want SEEK,RATE", *diskSpec)
		}
		hdd = &disk{seek: seek, rate: float64(r), lastEnd: -1}
	}
	dir := os.Args[1]
	files := http.FileServer(http.Dir(dir))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if *delay > 0 {
			time.Sleep(*delay)
		}
		if *holes404 && touchesHole(filepath.Join(dir, filepath.Clean("/"+r.URL.Path)), r.Header.Get("Range")) {
			http.NotFound(w, r)
			return
		}
		if hdd != nil {
			if start, end, ok := parseRange(r.Header.Get("Range")); ok {
				hdd.serve(start, end-start+1)
			}
		}
		cw := &counting{ResponseWriter: w, limiter: lim}
		files.ServeHTTP(cw, r)
		if *bytesLog != "" {
			bytesMu.Lock()
			if f, err := os.OpenFile(*bytesLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
				fmt.Fprintf(f, "%d\n", cw.n)
				f.Close()
			}
			bytesMu.Unlock()
		}
	})
	log.Fatal(http.ListenAndServe(os.Args[2], handler))
}

// parseRange reads a single "bytes=a-b" header.
func parseRange(header string) (start, end int64, ok bool) {
	spec, ok := strings.CutPrefix(header, "bytes=")
	a, b, ok2 := strings.Cut(spec, "-")
	if !ok || !ok2 {
		return 0, 0, false
	}
	start, err1 := strconv.ParseInt(a, 10, 64)
	end, err2 := strconv.ParseInt(b, 10, 64)
	return start, end, err1 == nil && err2 == nil
}

// touchesHole reports whether the single range of path overlaps a hole.
func touchesHole(path, header string) bool {
	start, end, ok := parseRange(header)
	if !ok {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	data, err := unix.Seek(int(f.Fd()), start, unix.SEEK_DATA)
	if errors.Is(err, unix.ENXIO) || (err == nil && data > start) {
		return true // start is in a hole (or past the last data)
	}
	if err != nil {
		return false
	}
	hole, err := unix.Seek(int(f.Fd()), start, unix.SEEK_HOLE)
	return err == nil && hole <= end
}
