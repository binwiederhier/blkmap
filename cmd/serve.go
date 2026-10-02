package cmd

import (
	"context"
	"errors"
	"log"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v2"

	"heckel.io/blkmap/device"
	"heckel.io/blkmap/util"
)

var (
	cmdServe = &cli.Command{
		Name:      "serve",
		Usage:     "Serve /dev/blkmap/ID from /etc/blkmap/ID.yml until interrupted",
		ArgsUsage: "ID",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "config", Aliases: []string{"c"}, Usage: "config `FILE` (default: /etc/blkmap/ID.yml)"},
		},
		Action: execServe,
	}
)

func execServe(c *cli.Context) error {
	conf, err := loadConfig(c.Args().First(), c.String("config"), false)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	d, err := device.Start(ctx, conf, device.DevDir)
	if err != nil {
		return err
	}
	log.Printf("serving %s (%s): %s, %d segments, %d/%d chunks in cow file %s",
		d.Path, d.BlockPath, util.FormatSize(d.Size()), len(conf.Segments), d.Written(), d.Chunks(), conf.COW.File)
	if _, err := util.SdNotify(util.NotifyReady); err != nil {
		log.Printf("sd_notify failed: %s", err.Error())
	}
	var failure error
	select {
	case <-ctx.Done():
		log.Printf("stopping %s", d.Path)
	case <-d.Done():
		// The kernel device is dead underneath; exit non-zero so systemd restarts the unit
		failure = d.Err()
		log.Printf("%s failed: %s; stopping", d.Path, failure.Error())
	}
	util.SdNotify(util.NotifyStopping)
	written := d.Written()
	if err := d.Close(); err != nil {
		return errors.Join(failure, err)
	}
	log.Printf("stopped %s, %d chunks in cow file", d.Path, written)
	return failure
}
