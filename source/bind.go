package source

import "io"

// Lookup resolves a sibling device of a group to its live view: what the device currently
// reads, COW overlay included. It returns false for an unknown id.
type Lookup func(id string) (io.ReaderAt, bool)

// Binder is implemented by sources that derive their bytes from other devices of the same
// group (a RAID member whose parity is the XOR of its siblings, a mirror plex that is a view
// of another plex). The group calls Bind with a Lookup before any device serves I/O, so the
// source can read its siblings as the guest sees them rather than from their bases.
type Binder interface {
	Bind(lookup Lookup)
}
