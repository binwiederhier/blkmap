package source

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"heckel.io/blkmap/util"
)

const (
	prefetchComment = "#"
	recordRead      = "R"
	recordWrite     = "W"
	// maxRanges bounds a list so a hostile or broken file cannot exhaust memory.
	maxRanges = 1 << 20
)

// Range is a byte range of the device address space.
type Range struct {
	Offset int64
	Length int64
}

// ParsePrefetch reads a prefetch list: one "offset length" range per line (binary size
// suffixes allowed), '#' comments, highest priority first. Recording lines ("millis R|W
// offset length", see ParseRecording) are accepted too, reads only, so a recording works
// as a prefetch list as it is; hydration copies each chunk once anyway.
func ParsePrefetch(r io.Reader) ([]Range, error) {
	ranges, _, err := ParsePrefetchTimed(r)
	return ranges, err
}

// ParsePrefetchTimed is ParsePrefetch that also returns when the recorded workload first
// read each range, when every line carries a timestamp (a recording, raw or compacted);
// otherwise the times are nil.
func ParsePrefetchTimed(r io.Reader) ([]Range, []time.Duration, error) {
	var ranges []Range
	var at []time.Duration
	timed := true
	err := scanLines(r, "prefetch list", func(fields []string) error {
		switch len(fields) {
		case 2:
			rg, err := parseRange(fields[0], fields[1])
			if err != nil {
				return err
			}
			ranges = append(ranges, rg)
			timed = false
		case 4:
			a, err := parseAccess(fields)
			if err != nil {
				return err
			}
			if !a.Write {
				ranges = append(ranges, Range{Offset: a.Offset, Length: a.Length})
				at = append(at, time.Duration(a.Millis)*time.Millisecond)
			}
		default:
			return errors.New(`expected "offset length" or "millis R|W offset length"`)
		}
		if len(ranges) > maxRanges {
			return fmt.Errorf("more than %d ranges", maxRanges)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if !timed {
		at = nil
	}
	return ranges, at, nil
}

// parseRanges reads "offset length" lines; what names the file kind in errors.
func parseRanges(r io.Reader, what string) ([]Range, error) {
	var ranges []Range
	err := scanLines(r, what, func(fields []string) error {
		if len(fields) != 2 {
			return errors.New(`expected "offset length"`)
		}
		rg, err := parseRange(fields[0], fields[1])
		if err != nil {
			return err
		}
		if len(ranges) == maxRanges {
			return fmt.Errorf("more than %d ranges", maxRanges)
		}
		ranges = append(ranges, rg)
		return nil
	})
	return ranges, err
}

// scanLines calls fn with the fields of every line that is not empty or a comment, and
// prefixes its errors with the file kind and line number.
func scanLines(r io.Reader, what string, fn func(fields []string) error) error {
	scanner := bufio.NewScanner(r)
	for n := 1; scanner.Scan(); n++ {
		line := scanner.Text()
		if i := strings.Index(line, prefetchComment); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if err := fn(fields); err != nil {
			return fmt.Errorf("%s line %d: %w", what, n, err)
		}
	}
	return scanner.Err()
}

func parseRange(offsetField, lengthField string) (Range, error) {
	offset, err := util.ParseSize(offsetField)
	if err != nil {
		return Range{}, fmt.Errorf("offset: %w", err)
	}
	length, err := util.ParseSize(lengthField)
	if err != nil {
		return Range{}, fmt.Errorf("length: %w", err)
	}
	if length == 0 {
		return Range{}, errors.New("length must be positive")
	}
	return Range{Offset: offset, Length: length}, nil
}

// parseAccess reads the four fields of a recording line.
func parseAccess(fields []string) (Access, error) {
	millis, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || millis < 0 {
		return Access{}, fmt.Errorf("timestamp: %q is not a number of milliseconds", fields[0])
	}
	var write bool
	switch fields[1] {
	case recordRead:
	case recordWrite:
		write = true
	default:
		return Access{}, fmt.Errorf("operation must be %s or %s, got %q", recordRead, recordWrite, fields[1])
	}
	rg, err := parseRange(fields[2], fields[3])
	if err != nil {
		return Access{}, err
	}
	return Access{Millis: millis, Write: write, Offset: rg.Offset, Length: rg.Length}, nil
}

// ParsePrefetchFileTimed is ParsePrefetchTimed on a file.
func ParsePrefetchFileTimed(path string) ([]Range, []time.Duration, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	ranges, at, err := ParsePrefetchTimed(f)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return ranges, at, nil
}

// ParsePrefetchFile is ParsePrefetch on a file.
func ParsePrefetchFile(path string) ([]Range, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ranges, err := ParsePrefetch(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return ranges, nil
}

// Access is one guest request of a recording, Millis after the recording started.
type Access struct {
	Millis int64
	Write  bool
	Offset int64
	Length int64
}

// ParseRecording reads a recording: "millis R|W offset length" lines (written by a device
// with record: in its config, or compacted by CompactRecording), '#' comments.
func ParseRecording(r io.Reader) ([]Access, error) {
	var accesses []Access
	err := scanLines(r, "recording", func(fields []string) error {
		if len(fields) != 4 {
			return errors.New(`expected "millis R|W offset length"`)
		}
		a, err := parseAccess(fields)
		if err != nil {
			return err
		}
		accesses = append(accesses, a)
		return nil
	})
	return accesses, err
}

// CompactRecording turns a recording into a prefetch list: reads only, each chunk once at
// its first read, chunk-aligned, consecutive chunks merged into one range that keeps the
// first read's timestamp. The order is the order the workload first needed the data.
func CompactRecording(accesses []Access, chunkSize int64) []Access {
	seen := make(map[int64]bool)
	var out []Access
	for _, a := range accesses {
		if a.Write {
			continue
		}
		for c := a.Offset / chunkSize; c <= (a.Offset+a.Length-1)/chunkSize; c++ {
			if seen[c] {
				continue
			}
			seen[c] = true
			if n := len(out); n > 0 && out[n-1].Offset+out[n-1].Length == c*chunkSize {
				out[n-1].Length += chunkSize
				continue
			}
			out = append(out, Access{Millis: a.Millis, Offset: c * chunkSize, Length: chunkSize})
		}
	}
	return out
}

// AppendAccess appends a's recording line to b.
func AppendAccess(b []byte, a Access) []byte {
	b = strconv.AppendInt(b, a.Millis, 10)
	if a.Write {
		b = append(b, ' ', 'W', ' ')
	} else {
		b = append(b, ' ', 'R', ' ')
	}
	b = strconv.AppendInt(b, a.Offset, 10)
	b = append(b, ' ')
	b = strconv.AppendInt(b, a.Length, 10)
	return append(b, '\n')
}
