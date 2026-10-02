// lib-dircache serves an image through a fast cache tier that is a directory of 1 MiB block
// files (missing file = miss, served by the slow tier), with background hydration that
// bypasses the cache and a periodic cache statistics line.
//
//	sudo ./lib-dircache -slow /path/to/image -cache /path/to/cachedir [-populate 0:8] [-id dc]
//
// -populate copies the given block range into the cache directory first, standing in for
// whatever external process fills the cache.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"heckel.io/blkmap/config"
	"heckel.io/blkmap/device"
	"heckel.io/blkmap/source"
)

const (
	statsEvery = 5 * time.Second
)

func main() {
	id := flag.String("id", "dc", "device name: /dev/blkmap/<id>")
	slowPath := flag.String("slow", "", "image file (the slow tier)")
	cacheDir := flag.String("cache", "", "directory of block files (the fast tier)")
	populate := flag.String("populate", "", "block range FROM:TO to copy into the cache before serving")
	flag.Parse()
	if *slowPath == "" || *cacheDir == "" {
		flag.Usage()
		os.Exit(2)
	}
	slow, err := source.OpenFile(*slowPath, 0, 0)
	if err != nil {
		log.Fatal(err)
	}
	fast := NewDirCache(*cacheDir, slow.Size())
	if *populate != "" {
		from, to, err := parseRange(*populate)
		if err != nil {
			log.Fatal(err)
		}
		if err := fast.Populate(slow, from, to); err != nil {
			log.Fatal(err)
		}
		log.Printf("populated blocks %d..%d of the cache", from, to)
	}
	cache := source.NewCache(fast, slow)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dev, err := device.Serve(ctx, &device.Options{
		ID:      *id,
		Base:    cache,
		COWFile: filepath.Join(config.DefaultStateDir, "example-"+*id+".cow"),
		Hydrate: &device.Hydrate{
			Rest:     true,
			Rate:     16 << 20,
			UseCache: config.CacheNever, // background copies come from the slow tier only
			Report:   statsEvery,
			OnProgress: func(p device.Progress) {
				s := cache.Stats()
				log.Printf("cache: %d hits, %d misses, %d failures; hydrated %d/%d chunks", s.Hits, s.Misses, s.Failures, p.Hydrated, p.Total)
			},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("serving %s (%s); Ctrl-C to stop", dev.Path, dev.BlockPath)
	<-ctx.Done()
	if err := dev.Close(); err != nil {
		log.Fatal(err)
	}
}

func parseRange(s string) (int64, int64, error) {
	a, b, ok := strings.Cut(s, ":")
	if !ok {
		return 0, 0, fmt.Errorf("expected FROM:TO, got %q", s)
	}
	from, err := strconv.ParseInt(a, 10, 64)
	if err != nil {
		return 0, 0, err
	}
	to, err := strconv.ParseInt(b, 10, 64)
	return from, to, err
}
