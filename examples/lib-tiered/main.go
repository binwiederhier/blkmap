// lib-tiered serves a device from two backends of your own: a fast tier that holds some
// blocks (an in-memory block store standing in for a key-value store or a local cache) in
// front of a slow remote (an image file behind an artificial round trip), with background
// hydration from code and a progress callback.
//
//	sudo ./lib-tiered -image disk.img -cached 0:32 -delay 20ms
//	sudo dd if=/dev/blkmap/tiered of=/dev/null bs=1M     # cached blocks fast, the rest slow
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"heckel.io/blkmap/config"
	"heckel.io/blkmap/device"
	"heckel.io/blkmap/source"
)

func main() {
	id := flag.String("id", "tiered", "device name: /dev/blkmap/<id>")
	image := flag.String("image", "", "image file behind the slow tier")
	cached := flag.String("cached", "0:16", "1 MiB blocks FROM:TO to put in the fast tier")
	delay := flag.Duration("delay", 20*time.Millisecond, "round trip of the slow tier")
	flag.Parse()
	if *image == "" {
		flag.Usage()
		os.Exit(2)
	}
	file, err := source.OpenFile(*image, 0, 0)
	if err != nil {
		log.Fatal(err)
	}
	slow := newSlowTier(file, *delay, "1")
	fast := newMemTier(slow.Size())
	var from, to int64
	if _, err := fmt.Sscanf(*cached, "%d:%d", &from, &to); err != nil {
		log.Fatalf("-cached: %s", err)
	}
	if err := fast.fill(file, from, to); err != nil {
		log.Fatal(err)
	}
	base := tiered(fast, slow)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dev, err := device.Serve(ctx, &device.Options{
		ID:       *id,
		Base:     base,
		COWFile:  filepath.Join(config.DefaultStateDir, "example-"+*id+".cow"),
		Identity: source.Identity(base),
		Hydrate: &device.Hydrate{
			Rest:   true,
			Rate:   32 << 20, // the rest phase at 32 MiB/s; on-demand reads come first
			Report: 5 * time.Second,
			OnProgress: func(p device.Progress) {
				st := base.Stats()
				log.Printf("hydration %s: %d/%d chunks; cache %d hits, %d misses", p.Phase, p.Hydrated, p.Total, st.Hits, st.Misses)
			},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("serving %s (%s); blocks %d..%d cached, the rest %s away; Ctrl-C to stop", dev.Path, dev.BlockPath, from, to-1, *delay)
	<-ctx.Done()
	if err := dev.Close(); err != nil { // aborts reads still waiting on the slow tier
		log.Fatal(err)
	}
}
