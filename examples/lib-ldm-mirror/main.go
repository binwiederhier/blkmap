// lib-ldm-mirror rebuilds both disks of a Windows dynamic-disk mirror from a backup of one
// of them, as two block devices served from one process (device.ServeGroup): /dev/blkmap/ldm0
// is the backed-up disk, /dev/blkmap/ldm1 is the other disk, whose mirrored data range is a
// live view of ldm0's with its own overlay. Hand both to a VM and Windows sees an intact
// mirror. With -demo it shows nopwrite: a resync that rewrites disk 1 with disk 0's
// bytes stores nothing, a mirrored write stores one chunk, and a write to disk 1 alone
// stays on disk 1.
//
//	sudo ./lib-ldm-mirror -disk0 disk0.img [-header1 disk1-header.img] [-data-offset 1M] [-demo]
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"heckel.io/blkmap/config"
	"heckel.io/blkmap/device"
	"heckel.io/blkmap/util"
)

func main() {
	disk0 := flag.String("disk0", "", "image of the backed-up disk")
	header1 := flag.String("header1", "", "disk 1's own header region, if you have it (default zeros)")
	dataOffset := flag.String("data-offset", "1M", "start of the mirrored volume extent on each disk")
	dataLength := flag.String("data-length", "", "length of the extent (default: up to the LDM database in the last MiB)")
	demo := flag.Bool("demo", false, "show nopwrite (identical writes skipped), then keep serving")
	flag.Parse()
	if *disk0 == "" {
		flag.Usage()
		os.Exit(2)
	}
	off, err := util.ParseSize(*dataOffset)
	if err != nil {
		log.Fatal(err)
	}
	info, err := os.Stat(*disk0)
	if err != nil {
		log.Fatal(err)
	}
	length := defaultDataLength(info.Size(), off)
	if *dataLength != "" {
		if length, err = util.ParseSize(*dataLength); err != nil {
			log.Fatal(err)
		}
	}
	m := &mirror{disk0: *disk0, header1: *header1, dataOffset: off, dataLength: length, stateDir: config.DefaultStateDir}
	opts, err := m.groupOptions()
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	g, err := device.ServeGroup(ctx, opts)
	if err != nil {
		log.Fatal(err)
	}
	d0, d1 := g.Devices["ldm0"], g.Devices["ldm1"]
	log.Printf("serving %s and %s; ldm1 %s..%s is a view of ldm0", d0.Path, d1.Path, util.FormatSize(off), util.FormatSizeApprox(off+length))
	if *demo {
		if err := showElision(d0, d1, off); err != nil {
			log.Printf("demo failed: %s", err.Error())
		}
	}
	<-ctx.Done()
	if err := g.Close(); err != nil {
		log.Fatal(err)
	}
}

// showElision does what Windows does to a restored mirror, through the kernel devices. A
// resync reads a stretch of disk 0 and writes it to disk 1 at the same place: nothing is
// stored. A mirrored write puts the same bytes on both disks: stored once, on disk 0, and
// disk 1 reads them through its view. A write to disk 1 alone, like a resync copy that is
// stale by the time it lands, is stored on disk 1 and leaves disk 0 untouched.
func showElision(d0, d1 *device.Device, off int64) error {
	f0, err := os.OpenFile(d0.BlockPath, os.O_RDWR|syscall.O_DIRECT, 0)
	if err != nil {
		return err
	}
	defer f0.Close()
	f1, err := os.OpenFile(d1.BlockPath, os.O_RDWR|syscall.O_DIRECT, 0)
	if err != nil {
		return err
	}
	defer f1.Close()
	buf := aligned(16 << 20)
	if _, err := f0.ReadAt(buf, off); err != nil {
		return err
	}
	before := d0.Written() + d1.Written()
	if _, err := f1.WriteAt(buf, off); err != nil { // the resync: identical bytes
		return err
	}
	if err := f1.Sync(); err != nil {
		return err
	}
	fmt.Printf("resync of 16 MiB onto ldm1: chunks stored before %d, after %d\n", before, d0.Written()+d1.Written())
	change := aligned(4096)
	copy(change, bytes.Repeat([]byte("changed "), 512))
	for _, f := range []*os.File{f0, f1} { // a mirrored write: the same bytes to both plexes
		if _, err := f.WriteAt(change, off); err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
	}
	back := aligned(4096)
	if _, err := f1.ReadAt(back, off); err != nil {
		return err
	}
	fmt.Printf("4 KiB mirrored write to both disks: chunks stored %d; ldm1 reads it back: %v\n", d0.Written()+d1.Written(), bytes.Equal(back, change))
	stale := aligned(4096)
	copy(stale, bytes.Repeat([]byte("stale   "), 512))
	if _, err := f1.WriteAt(stale, off); err != nil { // a write to disk 1 alone
		return err
	}
	if err := f1.Sync(); err != nil {
		return err
	}
	if _, err := f0.ReadAt(back, off); err != nil {
		return err
	}
	fmt.Printf("4 KiB write to ldm1 alone: chunks stored %d; ldm0 keeps its bytes: %v\n", d0.Written()+d1.Written(), bytes.Equal(back, change))
	return nil
}

// aligned returns a page-aligned buffer, as O_DIRECT requires.
func aligned(n int) []byte {
	b := make([]byte, n+4096)
	skip := (4096 - int(addr(b)%4096)) % 4096
	return b[skip : skip+n]
}
