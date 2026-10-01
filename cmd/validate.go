package cmd

import (
	"fmt"
	"strings"
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
	if conf.Hydrate != nil {
		line, err := describeHydrate(conf.Hydrate)
		if err != nil {
			return err
		}
		fmt.Fprintf(c.App.Writer, "Hydrate: %s\n", line)
	}
	fmt.Fprintf(c.App.Writer, "Layout:\n")
	w := tabwriter.NewWriter(c.App.Writer, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "  OFFSET\tSIZE\tSOURCE\n")
	var pos int64
	for i, s := range src.Segments() {
		if s.Offset > pos {
			fmt.Fprintf(w, "  %s\t%s\tgap (zeros)\n", util.FormatSize(pos), util.FormatSize(s.Offset-pos))
		}
		cs := conf.Segments[i]
		fmt.Fprintf(w, "  %s\t%s\t%s\n", util.FormatSize(s.Offset), util.FormatSize(s.Source.Size()), describe(cs))
		for j, m := range cs.Members {
			fmt.Fprintf(w, "  \tmember %d\t%s\n", j, describeNested(m))
		}
		if cs.Type == config.SourceCache {
			fmt.Fprintf(w, "  \tfast\t%s\n", describeNested(cs.Fast))
			fmt.Fprintf(w, "  \tslow\t%s\n", describeNested(cs.Slow))
		}
		pos = s.Offset + s.Source.Size()
	}
	if pos < src.Size() {
		fmt.Fprintf(w, "  %s\t%s\tgap (zeros)\n", util.FormatSize(pos), util.FormatSize(src.Size()-pos))
	}
	return w.Flush()
}

// describe renders a top-level segment for the layout table; nested parts follow on their
// own lines.
func describe(s *config.Segment) string {
	switch s.Type {
	case config.SourceRAID5:
		return fmt.Sprintf("raid5 (%d members, %s stripes, %s)", len(s.Members), util.FormatSize(s.StripeSize), s.Layout)
	case config.SourceCache:
		return "cache"
	}
	return describeNested(s)
}

// describeNested renders a source on one line, recursing into cache tiers.
func describeNested(s *config.Segment) string {
	var out string
	switch {
	case s.Missing:
		return "missing (reconstructed from parity)"
	case s.Type == config.SourceCache:
		out = fmt.Sprintf("cache (fast: %s, slow: %s)", describeNested(s.Fast), describeNested(s.Slow))
	case s.Type == config.SourceCustom:
		out = "custom " + s.Name
	case s.Type == config.SourceRAID5:
		out = fmt.Sprintf("raid5 (%d members)", len(s.Members))
	default:
		out = string(s.Type)
		switch s.Type {
		case config.SourceFile, config.SourceDevice:
			out += " " + s.Path
		case config.SourceHTTP:
			out += " " + s.URL
		}
	}
	if s.SourceOffset > 0 {
		out += fmt.Sprintf(" (offset %s)", util.FormatSize(s.SourceOffset))
	}
	return out
}

// describeHydrate summarizes the hydration plan, reading the prefetch list to validate it.
func describeHydrate(h *config.Hydrate) (string, error) {
	var parts []string
	if h.PrefetchList != "" {
		ranges, err := source.ParsePrefetchFile(h.PrefetchList)
		if err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("%d prefetch ranges from %s", len(ranges), h.PrefetchList))
	} else {
		parts = append(parts, "no prefetch list")
	}
	switch {
	case !h.Rest:
		parts = append(parts, "list only")
	case h.Rate > 0:
		parts = append(parts, fmt.Sprintf("then the rest at %s/s", util.FormatSize(h.Rate)))
	default:
		parts = append(parts, "then the rest unlimited")
	}
	parts = append(parts, "cache "+h.UseCache)
	return strings.Join(parts, ", "), nil
}
