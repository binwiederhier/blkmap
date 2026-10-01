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
		for j, m := range conf.Segments[i].Members {
			fmt.Fprintf(w, "  \tmember %d\t%s\n", j, describeMember(m))
		}
		pos = s.Offset + s.Source.Size()
	}
	if pos < src.Size() {
		fmt.Fprintf(w, "  %s\t%s\tgap (zeros)\n", util.FormatSize(pos), util.FormatSize(src.Size()-pos))
	}
	return w.Flush()
}

// describe renders a config segment's source for the layout table.
func describe(s *config.Segment) string {
	if s.Type == config.SourceRAID5 {
		return fmt.Sprintf("raid5 (%d members, %s stripes, %s)", len(s.Members), util.FormatSize(s.StripeSize), s.Layout)
	}
	return describeSource(s.Type, s.Path, s.URL, s.SourceOffset)
}

// describeMember renders one raid5 member for the layout table.
func describeMember(m *config.Member) string {
	if m.Missing {
		return "missing (reconstructed from parity)"
	}
	return describeSource(m.Type, m.Path, m.URL, m.SourceOffset)
}

func describeSource(typ config.SourceType, path, url string, sourceOffset int64) string {
	out := string(typ)
	switch typ {
	case config.SourceFile, config.SourceDevice:
		out += " " + path
	case config.SourceHTTP:
		out += " " + url
	}
	if sourceOffset > 0 {
		out += fmt.Sprintf(" (offset %s)", util.FormatSize(sourceOffset))
	}
	return out
}
