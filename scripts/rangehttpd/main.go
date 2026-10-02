// rangehttpd serves a directory over HTTP with Range support, an optional fixed delay per
// request (a stand-in for a slow link) and an optional log of bytes sent per response. It
// is the HTTP origin for blkmap's e2e, stress and example scripts; net/http serves Range
// requests concurrently and fast, which python's http.server does not.
//
//	rangehttpd DIR ADDR [-delay 20ms] [-bytes-log FILE]
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

var (
	bytesMu sync.Mutex
)

// counting wraps a ResponseWriter to log the body bytes of a response.
type counting struct {
	http.ResponseWriter
	n int64
}

func (c *counting) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}

func main() {
	fs := flag.NewFlagSet("rangehttpd", flag.ExitOnError)
	delay := fs.Duration("delay", 0, "delay added to every request")
	bytesLog := fs.String("bytes-log", "", "append the body size of every response to this file")
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: rangehttpd DIR ADDR [-delay 20ms] [-bytes-log FILE]")
		os.Exit(2)
	}
	fs.Parse(os.Args[3:])
	files := http.FileServer(http.Dir(os.Args[1]))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if *delay > 0 {
			time.Sleep(*delay)
		}
		cw := &counting{ResponseWriter: w}
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
