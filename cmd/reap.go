package cmd

import (
	"github.com/urfave/cli/v2"

	"heckel.io/blkmap/device"
)

var (
	cmdReap = &cli.Command{
		Name:      "reap",
		Usage:     "Delete the kernel device a dead server of ID left waiting (fails its I/O)",
		ArgsUsage: "ID",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "run-dir", Value: device.RunDir, Hidden: true},
		},
		Action: execReap,
	}
)

func execReap(c *cli.Context) error {
	return device.Reap(c.Args().First(), c.String("run-dir"))
}
