package cmd

import (
	"fmt"
	"text/tabwriter"

	"github.com/urfave/cli/v2"

	"heckel.io/blkmap/config"
	"heckel.io/blkmap/device"
	"heckel.io/blkmap/source"
	"heckel.io/blkmap/util"
)

var (
	cmdValidate = &cli.Command{
		Name:      "validate",
		Usage:     "Check a config, open its sources and print the resolved layout",
		ArgsUsage: "ID|FILE",
		Action:    execValidate,
	}
)

func execValidate(c *cli.Context) error {
	conf, err := loadConfig(c.Args().First(), "", true)
	if err != nil {
		return err
	}
	src, err := source.FromConfig(conf)
	if err != nil {
		return err
	}
	defer src.Close()
	mode := "read-write"
	if conf.ReadOnly {
		mode = "read-only"
	}
	fmt.Fprintf(c.App.Writer, "Device:  %s -> %s/%s\n", conf.ID, device.DevDir, conf.ID)
	fmt.Fprintf(c.App.Writer, "Size:    %s (block size %d, %s)\n", util.FormatSize(src.Size()), conf.BlockSize, mode)
	fmt.Fprintf(c.App.Writer, "COW:     %s (bitmap %s, chunk %s)\n", conf.COW.File, conf.COW.Bitmap, util.FormatSize(conf.COW.ChunkSize))
	fmt.Fprintf(c.App.Writer, "Layout:\n")
	w := tabwriter.NewWriter(c.App.Writer, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "  OFFSET\tSIZE\tSOURCE\n")
	var pos int64
	for i, s := range src.Segments() {
		if s.Offset > pos {
			fmt.Fprintf(w, "  %s\t%s\tgap (zeros)\n", util.FormatSize(pos), util.FormatSize(s.Offset-pos))
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\n", util.FormatSize(s.Offset), util.FormatSize(s.Source.Size()), describe(conf.Segments[i]))
		pos = s.Offset + s.Source.Size()
	}
	if pos < src.Size() {
		fmt.Fprintf(w, "  %s\t%s\tgap (zeros)\n", util.FormatSize(pos), util.FormatSize(src.Size()-pos))
	}
	return w.Flush()
}

// describe renders a config segment's source for the layout table.
func describe(s *config.Segment) string {
	out := string(s.Type)
	switch s.Type {
	case config.SourceFile, config.SourceDevice:
		out += " " + s.Path
	case config.SourceHTTP:
		out += " " + s.URL
	}
	if s.SourceOffset > 0 {
		out += fmt.Sprintf(" (offset %s)", util.FormatSize(s.SourceOffset))
	}
	return out
}
