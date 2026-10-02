package cmd

import (
	"errors"
	"fmt"

	"github.com/urfave/cli/v2"

	"heckel.io/blkmap/source"
	"heckel.io/blkmap/util"
)

var (
	cmdMap = &cli.Command{
		Name:      "map",
		Usage:     "Print the data extents of a file or device as a map (offset length per line)",
		ArgsUsage: "FILE",
		Action:    execMap,
	}
	errNoFile = errors.New("FILE argument required")
)

// execMap walks SEEK_DATA/SEEK_HOLE and prints what a map file needs, so a sparse image can
// be published together with its layout and hydrated without transferring its holes.
func execMap(c *cli.Context) error {
	path := c.Args().First()
	if path == "" {
		return errNoFile
	}
	src, err := source.OpenFile(path, 0, 0)
	if err != nil {
		return err
	}
	defer src.Close()
	extents, err := source.Extents(src)
	if err != nil {
		return err
	}
	var data int64
	for _, e := range extents {
		data += e.Length
	}
	fmt.Fprintf(c.App.Writer, "# data extents of %s: %d extents, %s data of %s total\n", path, len(extents), util.FormatSize(data), util.FormatSize(src.Size()))
	for _, e := range extents {
		fmt.Fprintf(c.App.Writer, "%s %s\n", util.FormatSize(e.Offset), util.FormatSize(e.Length))
	}
	return nil
}
