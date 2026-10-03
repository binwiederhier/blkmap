package source

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// FuzzParseRanges covers prefetch lists and maps: arbitrary text must parse or fail, and a
// map built from what parses must keep its extents sorted, merged and in range.
func FuzzParseRanges(f *testing.F) {
	for _, seed := range []string{"0 1M\n64K 128K # c\n", "", "#\n\n", "1 2 3\n", "x y\n", "0 0\n", "9223372036854775807 1\n", "4K 4K\n0 8K\n"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		ranges, err := parseRanges(strings.NewReader(text), "fuzz")
		if err != nil {
			return
		}
		for _, r := range ranges {
			if r.Length <= 0 || r.Offset < 0 {
				t.Fatalf("accepted range %+v", r)
			}
		}
		m, err := NewMap(ranges)
		if err != nil {
			return
		}
		ext := m.Extents()
		for i := range ext {
			if ext[i].Length <= 0 || (i > 0 && ext[i].Offset <= ext[i-1].Offset+ext[i-1].Length) {
				t.Fatalf("map extents not sorted and merged: %+v", ext)
			}
		}
	})
}

func FuzzParseContentRange(f *testing.F) {
	for _, seed := range []string{"bytes 0-0/100", "bytes 0-99/100", "bytes 5-4/100", "bytes 0-0/*", "bytes 0-100/100", "items 0-0/1", "", "bytes -1-0/5", "bytes 0-0/-1"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, cr string) {
		start, end, total, err := parseContentRange(cr)
		if err == nil && (start < 0 || end < start || total <= end) {
			t.Fatalf("parseContentRange(%q) accepted inconsistent %d-%d/%d", cr, start, end, total)
		}
	})
}

// FuzzHTTPReply answers block fetches with arbitrary status, Content-Range and body length;
// the source must never panic and must never return data of the wrong length.
func FuzzHTTPReply(f *testing.F) {
	f.Add(206, "bytes 0-1048575/2097152", 1048576)
	f.Add(206, "bytes 0-10/2097152", 11)
	f.Add(200, "", 2097152)
	f.Add(500, "", 0)
	f.Add(206, "garbage", 1048576)
	f.Add(206, "bytes 0-1048575/2097152", 17)
	f.Fuzz(func(t *testing.T, status int, contentRange string, bodyLen int) {
		if status < 100 || status > 599 || bodyLen < 0 || bodyLen > 4<<20 {
			return
		}
		const total = 2 * httpBlockSize
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Range") == httpProbeRange { // a sane probe, the fuzzing is about block replies
				w.Header().Set(httpContentRange, fmt.Sprintf("bytes 0-0/%d", total))
				w.WriteHeader(http.StatusPartialContent)
				w.Write([]byte{0})
				return
			}
			if contentRange != "" {
				w.Header().Set(httpContentRange, contentRange)
			}
			w.WriteHeader(status)
			w.Write(bytes.Repeat([]byte{7}, bodyLen))
		}))
		defer srv.Close()
		client := srv.Client()
		client.Timeout = 5 * time.Second
		h, err := NewHTTP(client, srv.URL, 0, 0)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		defer h.Close()
		p := make([]byte, 100)
		n, err := h.ReadAt(p, 50)
		if err == nil && n != len(p) {
			t.Fatalf("ReadAt returned %d bytes without error", n)
		}
	})
}
