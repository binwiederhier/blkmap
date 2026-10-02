package source

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"heckel.io/blkmap/util"
)

const (
	prefetchComment = "#"
	// maxRanges bounds a list so a hostile or broken file cannot exhaust memory.
	maxRanges = 1 << 20
)

// Range is a byte range of the device address space.
type Range struct {
	Offset int64
	Length int64
}

// ParsePrefetch reads a prefetch list: one "offset length" range per line (binary size
// suffixes allowed), '#' comments, highest priority first.
func ParsePrefetch(r io.Reader) ([]Range, error) {
	return parseRanges(r, "prefetch list")
}

// parseRanges reads "offset length" lines; what names the file kind in errors.
func parseRanges(r io.Reader, what string) ([]Range, error) {
	var ranges []Range
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
		if len(fields) != 2 {
			return nil, fmt.Errorf("%s line %d: expected \"offset length\", got %q", what, n, line)
		}
		offset, err := util.ParseSize(fields[0])
		if err != nil {
			return nil, fmt.Errorf("%s line %d: offset: %w", what, n, err)
		}
		length, err := util.ParseSize(fields[1])
		if err != nil {
			return nil, fmt.Errorf("%s line %d: length: %w", what, n, err)
		}
		if length == 0 {
			return nil, fmt.Errorf("%s line %d: length must be positive", what, n)
		}
		if len(ranges) == maxRanges {
			return nil, fmt.Errorf("%s: more than %d ranges", what, maxRanges)
		}
		ranges = append(ranges, Range{Offset: offset, Length: length})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return ranges, nil
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
