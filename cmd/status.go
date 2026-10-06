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
// describeOverlay says what the device keeps locally, for serve's log and status.
func describeOverlay(readOnly bool, cowFile string, written, chunks int64) string {
	percent := 100 * written / max(chunks, 1)
	switch {
	case cowFile == "":
		return "read-only, no cow file"
	case readOnly: // only hydration writes the cow file: a local copy of the base
		return fmt.Sprintf("read-only, %d/%d chunks hydrated to %s (%d%%)", written, chunks, cowFile, percent)
	default:
		return fmt.Sprintf("%d/%d chunks in the cow file (%d%%)", written, chunks, percent)
	}
}

func printStatus(w io.Writer, st *device.Status) {
	var notes []string
	if st.Recovered {
		notes = append(notes, "re-attached after a restart")
	}
	if st.Dirty {
		notes = append(notes, "unflushed writes")
	}
	fmt.Fprintf(w, "%s  %s -> %s  pid %d, up %s\n", st.ID, st.Path, st.BlockPath, st.PID, time.Since(st.Started).Round(time.Second))
	fmt.Fprintf(w, "  size %s, %s", util.FormatSize(st.Size), describeOverlay(st.ReadOnly, st.COWFile, st.Written, st.Chunks))
	if len(notes) > 0 {
		fmt.Fprintf(w, ", %s", strings.Join(notes, ", "))
	}
	fmt.Fprintln(w)
	r := st.IO
	fmt.Fprintf(w, "  io: %d reads (%s), %d writes (%s), %d flushes, %d errors, %d in flight\n",
		r.Reads, util.FormatSize(r.ReadBytes), r.Writes, util.FormatSize(r.WriteBytes), r.Flushes, r.Errors, r.Inflight)
	fmt.Fprintf(w, "  source: %d reads (%s), %d on demand (%s), %d errors, %s reading\n", st.Source.Reads, util.FormatSize(st.Source.Bytes), st.Source.DemandReads, util.FormatSize(st.Source.DemandBytes), st.Source.Errors, st.Source.Duration.Round(time.Millisecond))
	if c := st.Cache; c != nil {
		fmt.Fprintf(w, "  cache: %d hits, %d misses, %d failures\n", c.Hits, c.Misses, c.Failures)
	}
	if h := st.Hydration; h != nil {
		fmt.Fprintf(w, "  hydration %s: %d/%d chunks, %s copied, %d errors, %d listed chunks read on demand\n", h.Phase, h.Hydrated, h.Total, util.FormatSize(h.Copied), h.Errors, h.Late)
		if h.Schedule != nil {
			fmt.Fprintf(w, "  prefetch: %s\n", h.Schedule.String())
		}
	}
	if r := st.Reclaim; r != nil {
		fmt.Fprintf(w, "  reclaim: %d chunks (%s) dropped, %d examined, %d pending\n", r.Chunks, util.FormatSize(r.Bytes), r.Examined, r.Pending)
	}
	if r := st.Recording; r != nil {
		state := "done"
		if r.Active {
			state = "active"
		}
		fmt.Fprintf(w, "  recording %s to %s: %d requests, %d dropped\n", state, r.File, r.Requests, r.Dropped)
	}
	fmt.Fprintf(w, "  queues: %d (%d handing reads to workers)\n", st.Queues, st.ParallelQueues)
}
