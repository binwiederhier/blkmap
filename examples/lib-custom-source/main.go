// lib-custom-source serves a block device from a source your own program computes, wired
// in two ways: registered as a "type: custom" segment of an ordinary YAML config (so the
// rest of the device can be files, HTTP and so on, with hydration, recording and all), or
// handed straight to device.Serve.
//
//	sudo ./lib-custom-source -config logdisk.yml      # the registered segment type
//	sudo ./lib-custom-source -direct -size 256M       # device.Serve with the source
//	sudo head -c 64 /dev/blkmap/logdisk
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"heckel.io/blkmap/config"
	"heckel.io/blkmap/device"
	"heckel.io/blkmap/source"
	"heckel.io/blkmap/util"
)

var (
	errEOF = io.EOF
)

func main() {
	configPath := flag.String("config", "", "device config with a custom segment (see logdisk.yml)")
	direct := flag.Bool("direct", false, "serve the source with device.Serve instead of a config")
	id := flag.String("id", "logdisk", "device name for -direct: /dev/blkmap/<id>")
	size := flag.String("size", "256M", "device size for -direct")
	flag.Parse()
	// Register before any config that names the type is opened
	source.Register("logdisk", newLogDisk)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var dev *device.Device
	var err error
	switch {
	case *direct:
		n, perr := util.ParseSize(*size)
		if perr != nil {
			log.Fatal(perr)
		}
		base, _ := newLogDisk(n, map[string]string{"name": *id, "version": "1"})
		dev, err = device.Serve(ctx, &device.Options{
			ID:       *id,
			Base:     base,
			COWFile:  filepath.Join(config.DefaultStateDir, "example-"+*id+".cow"),
			Identity: source.Identity(base), // pins the overlay to this content
		})
	case *configPath != "":
		name := filepath.Base(*configPath)
		conf, lerr := config.Load(name[:len(name)-len(filepath.Ext(name))], *configPath)
		if lerr != nil {
			log.Fatal(lerr)
		}
		dev, err = device.Start(ctx, conf, device.DevDir)
	default:
		err = errors.New("pass -config FILE or -direct")
	}
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("serving %s (%s), %s; Ctrl-C to stop", dev.Path, dev.BlockPath, util.FormatSizeApprox(dev.Size()))
	<-ctx.Done()
	if err := dev.Close(); err != nil {
		log.Fatal(err)
	}
}
