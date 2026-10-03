package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/urfave/cli/v2"

	"heckel.io/blkmap/device"
	"heckel.io/blkmap/util"
)

var (
	// runDirFlag is the hidden test seam of the commands that read /run/blkmap.
	runDirFlag = &cli.StringFlag{Name: "run-dir", Value: device.RunDir, Hidden: true}
	cmdStatus  = &cli.Command{
		Name:      "status",
		Usage:     "Show the state of running devices",
		ArgsUsage: "[ID]",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "json", Usage: "print JSON"},
			runDirFlag,
		},
		Action: execStatus,
	}
	cmdMetrics = &cli.Command{
		Name:  "metrics",
		Usage: "Print metrics of all running devices in the Prometheus text format",
		Flags: []cli.Flag{
			runDirFlag,
		},
		Action: execMetrics,
	}
)

func execStatus(c *cli.Context) error {
	var statuses []*device.Status
	if id := c.Args().First(); id != "" {
		st, err := device.QueryStatus(c.String("run-dir"), id)
		if err != nil {
			return fmt.Errorf("%w (is blkmap@%s running?)", err, id)
		}
		statuses = append(statuses, st)
	} else {
		var err error
		if statuses, err = device.QueryAll(c.String("run-dir")); err != nil {
			return err
		}
	}
	if c.Bool("json") {
		enc := json.NewEncoder(c.App.Writer)
		for _, st := range statuses {
			if err := enc.Encode(st); err != nil {
				return err
			}
		}
		return nil
	}
	if len(statuses) == 0 {
		fmt.Fprintln(c.App.Writer, "no blkmap devices running")
	}
	for _, st := range statuses {
		printStatus(c.App.Writer, st)
	}
	return nil
}

func execMetrics(c *cli.Context) error {
	statuses, err := device.QueryAll(c.String("run-dir"))
	if err != nil {
		return err
	}
	return device.WriteMetrics(c.App.Writer, statuses...)
}

// printStatus writes one device's status for people.
func printStatus(w io.Writer, st *device.Status) {
	var notes []string
	if st.Recovered {
		notes = append(notes, "re-attached after a restart")
	}
	if st.Dirty {
		notes = append(notes, "unflushed writes")
	}
	fmt.Fprintf(w, "%s  %s -> %s  pid %d, up %s\n", st.ID, st.Path, st.BlockPath, st.PID, time.Since(st.Started).Round(time.Second))
	fmt.Fprintf(w, "  size %s, %d/%d chunks in the cow file (%d%%)", util.FormatSize(st.Size), st.Written, st.Chunks, 100*st.Written/max(st.Chunks, 1))
	if len(notes) > 0 {
		fmt.Fprintf(w, ", %s", strings.Join(notes, ", "))
	}
	fmt.Fprintln(w)
	r := st.IO
	fmt.Fprintf(w, "  io: %d reads (%s), %d writes (%s), %d flushes, %d errors, %d in flight\n",
		r.Reads, util.FormatSize(r.ReadBytes), r.Writes, util.FormatSize(r.WriteBytes), r.Flushes, r.Errors, r.Inflight)
	fmt.Fprintf(w, "  source: %d reads (%s), %d errors, %s reading\n", st.Source.Reads, util.FormatSize(st.Source.Bytes), st.Source.Errors, st.Source.Duration.Round(time.Millisecond))
	if c := st.Cache; c != nil {
		fmt.Fprintf(w, "  cache: %d hits, %d misses, %d failures\n", c.Hits, c.Misses, c.Failures)
	}
	if h := st.Hydration; h != nil {
		fmt.Fprintf(w, "  hydration %s: %d/%d chunks, %s copied, %d errors\n", h.Phase, h.Hydrated, h.Total, util.FormatSize(h.Copied), h.Errors)
	}
	fmt.Fprintf(w, "  queues: %d (%d handing reads to workers)\n", st.Queues, st.ParallelQueues)
}
