package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/urfave/cli/v2"

	"heckel.io/blkmap/config"
	"heckel.io/blkmap/source"
	"heckel.io/blkmap/util"
)

var (
	chunkSizeFlag = &cli.StringFlag{Name: "chunk-size", Value: util.FormatSize(config.DefaultChunkSize), Usage: "COW chunk `SIZE` of the device the list is for"}
	cmdRecording  = &cli.Command{
		Name:  "recording",
		Usage: "Work with a recording of guest I/O (the record: block of a config)",
		Subcommands: []*cli.Command{
			{
				Name:      "compact",
				Usage:     "Turn a recording into a prefetch list: reads only, each chunk once at its first read, merged",
				ArgsUsage: "RECORDING",
				Flags:     []cli.Flag{chunkSizeFlag},
				Action:    func(c *cli.Context) error { return execRecording(c, false) },
			},
			{
				Name:      "stats",
				Usage:     "Show how much data the recorded workload needed by when, and the rate that keeps ahead of it",
				ArgsUsage: "RECORDING",
				Flags:     []cli.Flag{chunkSizeFlag},
				Action:    func(c *cli.Context) error { return execRecording(c, true) },
			},
		},
	}
	// cmdPrefetch is the old name of "recording compact" (and "recording stats" with --stats),
	// kept hidden for one release.
	cmdPrefetch = &cli.Command{
		Name:      "prefetch",
		Hidden:    true,
		ArgsUsage: "RECORDING",
		Flags:     []cli.Flag{chunkSizeFlag, &cli.BoolFlag{Name: "stats"}},
		Action:    func(c *cli.Context) error { return execRecording(c, c.Bool("stats")) },
	}
	errNoRecording = errors.New("RECORDING argument required")
	// prefetchMarks are the points in time --stats reports cumulative needs at.
	prefetchMarks = []time.Duration{time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute}
)

// execRecording reads a recording and prints it compacted (reads only, each chunk once at its
// first read, consecutive chunks merged), or, with stats, its statistics.
func execRecording(c *cli.Context, stats bool) error {
	path := c.Args().First()
	if path == "" {
		return errNoRecording
	}
	chunkSize, err := util.ParseSize(c.String("chunk-size"))
	if err != nil || chunkSize <= 0 {
		return fmt.Errorf("invalid chunk size %q", c.String("chunk-size"))
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	accesses, err := source.ParseRecording(f)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	compact := source.CompactRecording(accesses, chunkSize)
	if stats {
		printPrefetchStats(c.App.Writer, accesses, compact)
		return nil
	}
	var buf []byte
	for _, a := range compact {
		buf = source.AppendAccess(buf, a)
	}
	_, err = c.App.Writer.Write(buf)
	return err
}

// printPrefetchStats reports the recording's size and how much unique data the workload had
// read by each mark, plus the constant hydration rate (from the start) that stays ahead of
// every read after the first second; the list phase runs uncapped, so that rate is what a
// listed prefetch has to achieve.
func printPrefetchStats(w io.Writer, accesses, compact []source.Access) {
	var reads, writes, last int64
	for _, a := range accesses {
		if a.Write {
			writes++
		} else {
			reads++
		}
		last = max(last, a.Millis)
	}
	duration := time.Duration(last) * time.Millisecond
	var unique int64
	for _, a := range compact {
		unique += a.Length
	}
	fmt.Fprintf(w, "requests:  %d (%d reads, %d writes) over %s\n", len(accesses), reads, writes, duration)
	fmt.Fprintf(w, "unique:    %s read, in %d ranges\n", util.FormatSizeApprox(unique), len(compact))
	fmt.Fprintf(w, "needed:\n")
	for _, mark := range prefetchMarks {
		var needed int64
		for _, a := range compact {
			if time.Duration(a.Millis)*time.Millisecond <= mark {
				needed += a.Length
			}
		}
		fmt.Fprintf(w, "  by %-6s %s\n", formatMark(mark), util.FormatSizeApprox(needed))
		if mark >= duration {
			break
		}
	}
	var cumulative int64
	var rate float64
	for _, a := range compact {
		cumulative += a.Length
		if a.Millis >= int64(time.Second/time.Millisecond) {
			rate = max(rate, float64(cumulative)/(float64(a.Millis)/1000))
		}
	}
	if rate == 0 {
		fmt.Fprintf(w, "rate:      everything was read within the first second\n")
		return
	}
	fmt.Fprintf(w, "rate:      %s/s from the start keeps ahead of the reads after the first second\n", util.FormatSizeApprox(int64(rate)))
}

// formatMark prints a mark the way a person writes it: 1s, 30s, 2m.
func formatMark(d time.Duration) string {
	if d >= time.Minute {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%ds", int(d/time.Second))
}
