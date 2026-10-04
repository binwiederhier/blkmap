package main

import (
	"fmt"
	"strconv"

	"heckel.io/blkmap/source"
)

const (
	logBlock  = 4096
	logStripe = 16 << 20 // one MiB of log lines at the start of every 16 MiB
	logData   = 1 << 20
)

// logDisk is a computed disk image: the first MiB of every 16 MiB holds text lines
// ("block N of <name>"), the rest is a hole. It shows the optional abilities a Source can
// have beyond ReadAt/Size/Close:
//
//   - source.Sparse: Holes tells blkmap where the zeros are, so hydration marks them
//     without reading and the read-ahead never fetches them
//   - source.Present: tells a cache whose fast tier this is which ranges it holds
//   - source.Identifier: a fingerprint of the content, recorded with the COW file; a
//     different fingerprint later means the overlay no longer fits and start is refused
type logDisk struct {
	name    string
	size    int64
	version string
}

var (
	_ source.Source     = (*logDisk)(nil)
	_ source.Sparse     = (*logDisk)(nil)
	_ source.Present    = (*logDisk)(nil)
	_ source.Identifier = (*logDisk)(nil)
)

// newLogDisk is a source.Constructor: config segments of type custom with name "logdisk"
// call it with their size and params.
func newLogDisk(size int64, params map[string]string) (source.Source, error) {
	if size <= 0 {
		return nil, fmt.Errorf("logdisk: size is required")
	}
	name := params["name"]
	if name == "" {
		name = "logdisk"
	}
	return &logDisk{name: name, size: size, version: params["version"]}, nil
}

func (d *logDisk) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	for n < len(p) && off+int64(n) < d.size {
		pos := off + int64(n)
		block := pos / logBlock
		end := min((block+1)*logBlock, d.size)
		chunk := p[n : n+int(min(int64(len(p)-n), end-pos))]
		if pos%logStripe < logData {
			line := d.line(block)
			for i := range chunk {
				chunk[i] = line[(pos+int64(i))%logBlock%int64(len(line))]
			}
		} else {
			clear(chunk)
		}
		n += len(chunk)
	}
	if n < len(p) {
		return n, fmt.Errorf("logdisk: read past the end: %w", errEOF)
	}
	return n, nil
}

// line is the text repeated through a block.
func (d *logDisk) line(block int64) []byte {
	return []byte("block " + strconv.FormatInt(block, 10) + " of " + d.name + "\n")
}

func (d *logDisk) Holes(off, length int64) ([]source.Range, error) {
	var holes []source.Range
	end := min(off+length, d.size)
	for stripe := off / logStripe * logStripe; stripe < end; stripe += logStripe {
		start, stop := max(stripe+logData, off), min(stripe+logStripe, end)
		if start < stop {
			holes = append(holes, source.Range{Offset: start, Length: stop - start})
		}
	}
	return holes, nil
}

// Present: everything is "present" here (holes are real zeros, not missing data). A cache's
// fast tier that lacks blocks would answer false for them and the cache would ask the slow
// tier instead.
func (d *logDisk) Present(off, length int64) bool {
	return off >= 0 && off+length <= d.size
}

func (d *logDisk) Identity() string {
	return fmt.Sprintf("logdisk:%s:%s:%d", d.name, d.version, d.size)
}

func (d *logDisk) Size() int64 {
	return d.size
}

func (d *logDisk) Close() error {
	return nil
}
