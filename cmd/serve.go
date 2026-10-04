package cmd

import (
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v2"

	"heckel.io/blkmap/cow"
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
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
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
	case <-hup:
		// systemctl reload (and package upgrades): make everything durable and re-execute
		// the binary in this process, which re-attaches to the device; I/O pauses for the
		// moment it takes, and systemd sees the same main process throughout
		log.Printf("re-executing to hand %s to a fresh server", d.Path)
		util.SdNotify(util.NotifyReloading)
		if err := d.Detach(); err != nil {
			log.Printf("detach: %s", err.Error())
		}
		reexec()
	case <-ctx.Done():
		log.Printf("stopping %s", d.Path)
	case <-d.Done():
		failure = d.Err()
		if errors.Is(failure, cow.ErrCOWFailed) {
			// Writes the guest saw complete may be lost with the COW file's page cache: hand
			// the device to a fresh server, which serves the last durable state
			log.Printf("%s failed: %s; restarting from the last flushed state", d.Path, failure.Error())
			if err := d.Abandon(); errors.Is(err, device.ErrNoRecovery) {
				break
			} else if err != nil && !errors.Is(err, cow.ErrCOWFailed) {
				log.Printf("abandon: %s", err.Error())
			}
			os.Exit(device.ExitDetached)
		}
		// The kernel device is dead underneath; exit non-zero so systemd restarts the unit
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

// reexec replaces the process image with the installed binary (the new one after an
// upgrade). If that fails, exiting with ExitDetached makes systemd start a new server.
func reexec() {
	path, err := exec.LookPath(os.Args[0])
	if err == nil {
		err = syscall.Exec(path, os.Args, os.Environ())
	}
	log.Printf("cannot re-execute %s: %s; exiting for a restart", os.Args[0], err.Error())
	os.Exit(device.ExitDetached)
}
