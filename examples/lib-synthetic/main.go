// lib-synthetic serves a block device whose content is computed on the fly: block i of 4 KiB
// is filled with the byte value i. It is the smallest possible library use of blkmap: one
// source.Source and one device.Serve call. Writes go to the COW file like for any device.
//
//	sudo ./lib-synthetic [-size 1G] [-id synth]
//	sudo dd if=/dev/blkmap/synth bs=4k count=3 | xxd | head
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"heckel.io/blkmap/device"
	"heckel.io/blkmap/util"
)

const (
	blockSize = 4096
)

// synthetic is a read-only source.Source: block i reads as byte(i).
type synthetic struct {
	size int64
}

func (s *synthetic) ReadAt(p []byte, off int64) (int, error) {
	for i := range p {
		p[i] = byte((off + int64(i)) / blockSize)
	}
	return len(p), nil
}

func (s *synthetic) Size() int64 {
	return s.size
}

func (s *synthetic) Close() error {
	return nil
}

func main() {
	id := flag.String("id", "synth", "device name: /dev/blkmap/<id>")
	size := flag.String("size", "256M", "device size")
	flag.Parse()
	n, err := util.ParseSize(*size)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dev, err := device.Serve(ctx, &device.Options{
		ID:      *id,
		Base:    &synthetic{size: n},
		COWFile: "/var/tmp/blkmap-example-" + *id + ".cow",
	})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("serving %s (%s), %s; Ctrl-C to stop", dev.Path, dev.BlockPath, *size)
	<-ctx.Done()
	if err := dev.Close(); err != nil {
		log.Fatal(err)
	}
}
