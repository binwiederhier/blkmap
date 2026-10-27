package cmd

import (
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"sync/atomic"
	"syscall"

	"github.com/urfave/cli/v2"

	"heckel.io/blkmap/cow"
	"heckel.io/blkmap/device"
	"heckel.io/blkmap/ublk"
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
	// Fed from the start, so a re-executed server keeps feeding it while it re-attaches; once
	// serving, only while the device is healthy, and through the stop as well: a server too
	// wedged to hand its device over is killed, and the kernel fails or requeues its requests
	var served atomic.Pointer[device.Device]
	healthy := func() bool {
		d := served.Load()
		return d == nil || d.Healthy()
	}
	watchdogCtx, stopWatchdog := context.WithCancel(context.Background())
	defer stopWatchdog()
	go util.Watchdog(watchdogCtx, healthy)
	d, err := device.Start(ctx, conf, device.DevDir)
	if err != nil {
		return err
	}
	served.Store(d)
	log.Printf("serving %s (%s): %s, %d segments, %s",
		d.Path, d.BlockPath, util.FormatSize(d.Size()), len(conf.Segments), describeOverlay(conf.ReadOnly, d.COWFile(), d.Written(), d.Chunks()))
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
		// Writes the guest saw complete may be lost with the COW file's page cache, or a
		// request is stuck for good (STOP_DEV would wait for it forever): hand the device to
		// a fresh server, which serves the last durable state and gets the stuck requests
		// reissued. Without recovery, Close leaves a stuck device to the exit, which fails them.
		if errors.Is(failure, cow.ErrCOWFailed) || errors.Is(failure, device.ErrStalled) || errors.Is(failure, ublk.ErrRequestLost) {
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
	log.Printf("stopped %s, %s", d.Path, describeOverlay(conf.ReadOnly, d.COWFile(), written, d.Chunks()))
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
