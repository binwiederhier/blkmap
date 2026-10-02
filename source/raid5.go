package source

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

// Layout is a RAID-5 parity rotation algorithm, named as Linux md does. Windows dynamic
// disks (LDM) use LeftSymmetric (libldm builds a "raid5_ls" dm table for them), which is
// also md's default.
type Layout int

const (
	LeftSymmetric Layout = iota
	LeftAsymmetric
	RightSymmetric
	RightAsymmetric
)

// RAID5 reassembles a RAID-5 array from its members. One member may be nil (missing) and is
// reconstructed from the others' parity.
type RAID5 struct {
	members    []Source
	stripeSize int64
	layout     Layout
	size       int64
	bufs       sync.Pool // stripe-sized scratch buffers for reconstruction
}

// NewRAID5 builds the array. members are in array order; a nil entry is a missing disk. A
// size of 0 means the full capacity, (len(members)-1) times the smallest member rounded down
// to whole stripes.
func NewRAID5(members []Source, stripeSize int64, layout Layout, size int64) (*RAID5, error) {
	n := len(members)
	if n < 3 {
		return nil, fmt.Errorf("raid5 needs at least 3 members, got %d", n)
	}
	if stripeSize <= 0 {
		return nil, fmt.Errorf("stripe size must be positive")
	}
	// Capacity follows the smallest present member, in whole rows
	missing := 0
	var smallest int64 = -1
	for _, m := range members {
		if m == nil {
			missing++
		} else if smallest < 0 || m.Size() < smallest {
			smallest = m.Size()
		}
	}
	if missing > 1 {
		return nil, fmt.Errorf("raid5 can reconstruct at most one missing member, %d are missing", missing)
	}
	full := (smallest / stripeSize) * stripeSize * int64(n-1)
	if size == 0 {
		size = full
	}
	if size%stripeSize != 0 {
		return nil, fmt.Errorf("raid5 size %d must be a multiple of the stripe size %d", size, stripeSize)
	}
	if size > full {
		return nil, fmt.Errorf("raid5 size %d exceeds the array capacity %d", size, full)
	}
	r := &RAID5{members: members, stripeSize: stripeSize, layout: layout, size: size}
	r.bufs.New = func() any {
		b := make([]byte, stripeSize)
		return &b
	}
	return r, nil
}

func (r *RAID5) ReadAt(p []byte, off int64) (int, error) {
	return r.readAt(p, off, false)
}

// ReadAtDirect reads every member around its cache tier, if it has one.
func (r *RAID5) ReadAtDirect(p []byte, off int64) (int, error) {
	return r.readAt(p, off, true)
}

func (r *RAID5) readAt(p []byte, off int64, direct bool) (int, error) {
	n, eof := clampRead(len(p), off, r.size)
	p = p[:n]
	for len(p) > 0 {
		unit := off / r.stripeSize
		within := off % r.stripeSize
		m := int(min(int64(len(p)), r.stripeSize-within))
		d, _ := r.locate(unit)
		memberOff := unit/int64(len(r.members)-1)*r.stripeSize + within
		var err error
		if r.members[d] != nil {
			err = r.readMember(d, p[:m], memberOff, direct)
		} else {
			err = r.reconstruct(d, p[:m], memberOff, direct)
		}
		if err != nil {
			return n - len(p), err
		}
		p = p[m:]
		off += int64(m)
	}
	return n, eof
}

func (r *RAID5) Size() int64 {
	return r.size
}

// Close closes every present member and returns the first error.
func (r *RAID5) Close() error {
	var errs []error
	for _, m := range r.members {
		if m != nil {
			errs = append(errs, m.Close())
		}
	}
	return errors.Join(errs...)
}

// locate maps a data stripe unit index to the member holding it and the parity member for
// that row, per the layout. Exposed for tests, which check it against md's definitions.
func (r *RAID5) locate(unit int64) (dataMember, parityMember int) {
	n := int64(len(r.members))
	row := unit / (n - 1)
	i := unit % (n - 1)
	var parity int64
	switch r.layout {
	case LeftSymmetric, LeftAsymmetric:
		parity = n - 1 - row%n // parity walks right to left
	default:
		parity = row % n // parity walks left to right
	}
	var data int64
	switch r.layout {
	case LeftSymmetric, RightSymmetric:
		data = (parity + 1 + i) % n // data starts after the parity member and wraps
	default:
		data = i // data fills the non-parity members left to right
		if data >= parity {
			data++
		}
	}
	return int(data), int(parity)
}

// readMember reads exactly len(p) bytes from member d.
func (r *RAID5) readMember(d int, p []byte, off int64, direct bool) error {
	var read int
	var err error
	if direct {
		read, err = ReadDirect(r.members[d], p, off)
	} else {
		read, err = r.members[d].ReadAt(p, off)
	}
	if err != nil && !(errors.Is(err, io.EOF) && read == len(p)) {
		return fmt.Errorf("raid5 member %d: %w", d, err)
	}
	if read < len(p) {
		return fmt.Errorf("raid5 member %d: short read at %d", d, off)
	}
	return nil
}

// reconstruct rebuilds the missing member d's bytes at off by XORing every other member.
func (r *RAID5) reconstruct(d int, p []byte, off int64, direct bool) error {
	clear(p)
	scratch := r.bufs.Get().(*[]byte)
	defer r.bufs.Put(scratch)
	buf := (*scratch)[:len(p)]
	for i := range r.members {
		if i == d {
			continue
		}
		if err := r.readMember(i, buf, off, direct); err != nil {
			return err
		}
		for j := range p {
			p[j] ^= buf[j]
		}
	}
	return nil
}
