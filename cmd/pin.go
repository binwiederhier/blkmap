package cmd

import (
	"fmt"

	"github.com/urfave/cli/v2"

	"heckel.io/blkmap/cow"
	"heckel.io/blkmap/source"
)

var (
	cmdPin = &cli.Command{
		Name:      "pin",
		Usage:     "Accept the current sources of a stopped device whose sources changed",
		ArgsUsage: "ID",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "config", Aliases: []string{"c"}, Usage: "config `FILE` (default: /etc/blkmap/ID.yml)"},
		},
		Action: execPin,
	}
)

func execPin(c *cli.Context) error {
	conf, err := loadConfig(c.Args().First(), c.String("config"), false)
	if err != nil {
		return err
	}
	if !conf.HasOverlay() {
		fmt.Fprintf(c.App.Writer, "%s is read-only without hydration: no cow file, nothing to pin\n", conf.ID)
		return nil
	}
	base, err := source.FromConfig(conf)
	if err != nil {
		return err
	}
	identity := source.Identity(base)
	base.Close()
	if err := cow.Pin(conf.COW.File, conf.COW.Bitmap, identity); err != nil {
		return err
	}
	fmt.Fprintf(c.App.Writer, "pinned %s to its current sources: %s\n", conf.ID, identity)
	return nil
}
