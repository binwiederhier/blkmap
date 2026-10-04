package main

import (
	"path/filepath"

	"heckel.io/blkmap/device"
	"heckel.io/blkmap/source"
)

// mirror describes a Windows dynamic-disk (LDM) mirrored volume, RAID-1 across two disks,
// of which only disk 0 was backed up. Each disk has its own partition table and LDM
// metadata (the 1 MiB private region and database are per disk); the volume's data lives on
// both disks at a data offset, byte for byte identical. So disk 1 is rebuilt as: its own
// header (from a small file if you have it, else zeros), and for the data range a view of
// disk 0's data range.
type mirror struct {
	disk0      string // image of the backed-up disk
	header1    string // disk 1's own header region (partition table, LDM private header); "" = zeros
	dataOffset int64  // where the mirrored volume's extent starts on each disk
	dataLength int64  // its length
	stateDir   string // where the COW files go
}

// groupOptions builds the two devices. Writes into disk 1's data range go to disk 0's
// store (the alias), so the plexes cannot diverge and nothing is stored twice. Both devices
// elide identical writes: when Windows resyncs the mirror after the restore, it rewrites
// every block of disk 1 with what disk 0 holds, and those writes cost no overlay space.
func (m *mirror) groupOptions() ([]*device.GroupOptions, error) {
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
	// Disk 1's own bytes: the header, then zeros; the data range never reads them (the
	// alias covers it), the LDM database at the end of the disk does
	base1, err := source.NewConcat([]*source.Segment{{Offset: 0, Source: header}, {Offset: m.dataOffset, Source: source.NewZero(size - m.dataOffset)}}, size)
	if err != nil {
		base0.Close()
		header.Close()
		return nil, err
	}
	opt := func(id string, base source.Source) device.Options {
		return device.Options{ID: id, Base: base, COWFile: filepath.Join(m.stateDir, "example-"+id+".cow"), Identity: source.Identity(base)}
	}
	return []*device.GroupOptions{
		{Options: opt("ldm0", base0), ElideIdenticalWrites: true},
		{Options: opt("ldm1", base1), ElideIdenticalWrites: true, Aliases: []device.Alias{
			{Offset: m.dataOffset, Length: m.dataLength, Target: "ldm0", TargetOffset: m.dataOffset},
		}},
	}, nil
}

// defaultDataLength is the volume extent when none is given: from the data offset to the
// LDM database, which takes the last MiB of a dynamic disk.
func defaultDataLength(diskSize, dataOffset int64) int64 {
	return diskSize - dataOffset - ldmDatabase
}

const (
	ldmDatabase = 1 << 20
)
