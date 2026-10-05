package main

import (
	"fmt"
	"io"
	"path/filepath"

	"heckel.io/blkmap/device"
	"heckel.io/blkmap/source"
)

// mirror describes a Windows dynamic-disk (LDM) mirrored volume, RAID-1 across two disks,
// of which only disk 0 was backed up. Each disk has its own partition table and LDM
// metadata (the 1 MiB private region and database are per disk); the volume's data lives on
// both disks at a data offset, byte for byte identical. So disk 1 is rebuilt as: its own
// header (from a small file if you have it, else zeros), and for the data range a live view
// of disk 0's data range.
type mirror struct {
	disk0      string // image of the backed-up disk
	header1    string // disk 1's own header region (partition table, LDM private header); "" = zeros
	dataOffset int64  // where the mirrored volume's extent starts on each disk
	dataLength int64  // its length
	stateDir   string // where the COW files go
}

// plexView is disk 1's mirrored range: it reads the same range of disk 0 through disk 0's
// device, overlay included, once the group has bound it (source.Binder). Disk 1's own COW
// overlay sits on top, so a write to disk 1 lands there and never on disk 0, exactly as with
// two real disks. With NopWrite a write to disk 1 that equals what disk 0 holds
// (Windows' resync, or disk 1's half of a mirrored write) stores nothing and the range keeps
// following disk 0; only a write that differs, such as a resync copy that is stale by the
// time it lands, is stored, and only on disk 1.
type plexView struct {
	target         string // device id of disk 0
	offset, length int64  // the range on disk 0
	lookup         source.Lookup
}

func (v *plexView) Bind(lookup source.Lookup) { v.lookup = lookup }

func (v *plexView) ReadAt(p []byte, off int64) (int, error) {
	if off >= v.length {
		return 0, io.EOF
	}
	var eof error
	if int64(len(p)) > v.length-off {
		p, eof = p[:v.length-off], io.EOF
	}
	if v.lookup == nil {
		return 0, fmt.Errorf("%s is not bound yet", v.target)
	}
	r, ok := v.lookup(v.target)
	if !ok {
		return 0, fmt.Errorf("%s is not served", v.target)
	}
	n, err := r.ReadAt(p, v.offset+off)
	if err == nil {
		err = eof
	}
	return n, err
}

func (v *plexView) Size() int64  { return v.length }
func (v *plexView) Close() error { return nil }

// groupOptions builds the two devices. Disk 1's data range is a plexView of disk 0's, each
// disk has its own overlay, and both skip identical writes (nopwrite): when Windows resyncs the mirror
// after the restore it rewrites every block of disk 1 with what disk 0 holds, and those
// writes cost no overlay space.
func (m *mirror) groupOptions() ([]*device.Options, error) {
	base0, err := source.OpenFile(m.disk0, 0, 0)
	if err != nil {
		return nil, err
	}
	size := base0.Size()
	header := source.Source(source.NewZero(m.dataOffset))
	if m.header1 != "" {
		if header, err = source.OpenFile(m.header1, 0, m.dataOffset); err != nil {
			base0.Close()
			return nil, err
		}
	}
	// Disk 1's own bytes: the header, the view of disk 0's data range, then zeros where its
	// LDM database at the end of the disk goes
	segments := []*source.Segment{
		{Offset: 0, Source: header},
		{Offset: m.dataOffset, Source: &plexView{target: "ldm0", offset: m.dataOffset, length: m.dataLength}},
	}
	if rest := size - m.dataOffset - m.dataLength; rest > 0 {
		segments = append(segments, &source.Segment{Offset: m.dataOffset + m.dataLength, Source: source.NewZero(rest)})
	}
	base1, err := source.NewConcat(segments, size)
	if err != nil {
		base0.Close()
		header.Close()
		return nil, err
	}
	opt := func(id string, base source.Source) *device.Options {
		return &device.Options{ID: id, Base: base, COWFile: filepath.Join(m.stateDir, "example-"+id+".cow"), Identity: source.Identity(base), NopWrite: true}
	}
	return []*device.Options{opt("ldm0", base0), opt("ldm1", base1)}, nil
}

// defaultDataLength is the volume extent when none is given: from the data offset to the
// LDM database, which takes the last MiB of a dynamic disk.
func defaultDataLength(diskSize, dataOffset int64) int64 {
	return diskSize - dataOffset - ldmDatabase
}

const (
	ldmDatabase = 1 << 20
)
