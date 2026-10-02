// grpc-remote exports sparse files from one host and mounts one of them as a block device on
// another, over a two-call gRPC protocol: Open (size and data-extent map) and Read. The
// client is a source.Source with holes attached, so blkmap serves holes locally and
// hydration transfers only the data.
//
//	host A:  ./grpc-remote serve -dir /srv/images -listen :9555
//	host B:  sudo ./grpc-remote mount -server hostA:9555 -name disk.img -id remote [-hydrate]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"heckel.io/blkmap/config"
	"heckel.io/blkmap/device"
	"heckel.io/blkmap/examples/grpc-remote/remotepb"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "mount":
		mount(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: grpc-remote serve -dir DIR [-listen :9555]\n       grpc-remote mount -server HOST:PORT -name FILE [-id remote] [-hydrate]")
	os.Exit(2)
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dir := fs.String("dir", "", "directory to export")
	listen := fs.String("listen", ":9555", "listen address")
	fs.Parse(args)
	if *dir == "" {
		fs.Usage()
		os.Exit(2)
	}
	l, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	s := grpc.NewServer(grpc.MaxSendMsgSize(2*maxRead), grpc.MaxConcurrentStreams(maxStreams))
	remotepb.RegisterRemoteServer(s, &server{dir: *dir})
	log.Printf("exporting %s on %s", *dir, *listen)
	if err := s.Serve(l); err != nil {
		log.Fatal(err)
	}
}

func mount(args []string) {
	fs := flag.NewFlagSet("mount", flag.ExitOnError)
	addr := fs.String("server", "", "server address HOST:PORT")
	name := fs.String("name", "", "exported file name")
	id := fs.String("id", "remote", "device name: /dev/blkmap/<id>")
	hydrate := fs.Bool("hydrate", false, "copy the data into the cow file in the background")
	fs.Parse(args)
	if *addr == "" || *name == "" {
		fs.Usage()
		os.Exit(2)
	}
	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(2*maxRead)))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	src, err := openRemote(ctx, conn, *name)
	if err != nil {
		log.Fatal(err)
	}
	opts := &device.Options{ID: *id, Base: src, COWFile: filepath.Join(config.DefaultStateDir, "example-"+*id+".cow")}
	if *hydrate {
		opts.Hydrate = &device.Hydrate{Rest: true, Report: 2 * time.Second}
	}
	dev, err := device.Serve(ctx, opts)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("serving %s (%s) from %s on %s; Ctrl-C to stop", dev.Path, dev.BlockPath, *name, *addr)
	<-ctx.Done()
	if err := dev.Close(); err != nil {
		log.Fatal(err)
	}
}
