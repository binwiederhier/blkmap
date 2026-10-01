// blkmap serves block devices stitched together from files, devices, HTTP sources and
// zeros, with writes captured in a copy-on-write file. See cmd for the CLI.
package main

import (
	"os"

	"heckel.io/blkmap/cmd"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	cmd.New(version, commit, date).Run(os.Args)
}
