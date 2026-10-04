// Package cmd implements the blkmap command line interface.
package cmd

import (
	"fmt"
	"os"

	"github.com/urfave/cli/v2"
)

// New returns the blkmap CLI application.
func New(version, commit, date string) *cli.App {
	return &cli.App{
		Name:  "blkmap",
		Usage: "Stitch files, devices and HTTP sources into a copy-on-write block device",
		Commands: []*cli.Command{
			cmdServe,
			cmdValidate,
			cmdMap,
			cmdRecording,
			cmdPrefetch,
			cmdUdevName,
			cmdReap,
			cmdPin,
			cmdStatus,
			cmdMetrics,
		},
		Version:         fmt.Sprintf("%s (%s, %s)", version, commit, date),
		HideHelpCommand: true,
		ExitErrHandler: func(_ *cli.Context, err error) {
			if err == nil {
				return
			}
			fmt.Fprintf(os.Stderr, "blkmap: %s\n", err.Error())
			os.Exit(1)
		},
	}
}
