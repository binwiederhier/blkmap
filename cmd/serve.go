package cmd

import (
	"context"
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
	log.Printf("serving %s (%s): %s, %d segments, %d chunks in cow file %s",
		d.Path, d.BlockPath, util.FormatSize(conf.Size), len(conf.Segments), d.Written(), conf.COW.File)
	if _, err := util.SdNotify(util.NotifyReady); err != nil {
		log.Printf("sd_notify failed: %s", err.Error())
	}
	<-ctx.Done()
	log.Printf("stopping %s", d.Path)
	util.SdNotify(util.NotifyStopping)
	written := d.Written()
	if err := d.Close(); err != nil {
		return err
	}
	log.Printf("stopped %s, %d chunks in cow file", d.Path, written)
	return nil
}
