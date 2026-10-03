package cmd

import (
	"fmt"

	"github.com/urfave/cli/v2"

	"heckel.io/blkmap/device"
)

var (
	cmdUdevName = &cli.Command{
		Name:      "udev-name",
		Usage:     "Print the blkmap id served as a ublk disk (called by the udev rule)",
		ArgsUsage: "KERNEL-NAME",
		Hidden:    true,
		Flags: []cli.Flag{
			runDirFlag,
		},
		Action: execUdevName,
	}
)

func execUdevName(c *cli.Context) error {
	name, err := device.UdevName(c.String("run-dir"), c.Args().First())
	if err != nil {
		return err
	}
	fmt.Fprintln(c.App.Writer, name)
	return nil
}
