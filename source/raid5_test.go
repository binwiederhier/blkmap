package source

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	raidStripe = 1024
)

// TestRAID5Locate pins the four rotations to Linux md's definitions (drivers/md/raid5.c) for
// a 3-disk array, rows 0..2, data units D0..D5. Columns: data member, parity member.
func TestRAID5Locate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		layout   Layout
		expected [6][2]int
	}{
		// Parity walks right to left; data fills the remaining members left to right
		{LeftAsymmetric, [6][2]int{{0, 2}, {1, 2}, {0, 1}, {2, 1}, {1, 0}, {2, 0}}},
		// Parity walks right to left; data starts just after the parity member and wraps
		{LeftSymmetric, [6][2]int{{0, 2}, {1, 2}, {2, 1}, {0, 1}, {1, 0}, {2, 0}}},
		// Parity walks left to right; data fills the remaining members left to right
		{RightAsymmetric, [6][2]int{{1, 0}, {2, 0}, {0, 1}, {2, 1}, {0, 2}, {1, 2}}},
		// Parity walks left to right; data starts just after the parity member and wraps
		{RightSymmetric, [6][2]int{{1, 0}, {2, 0}, {2, 1}, {0, 1}, {0, 2}, {1, 2}}},
	}
	for _, tt := range tests {
		r := &RAID5{members: make([]Source, 3), stripeSize: raidStripe, layout: tt.layout}
		for unit, want := range tt.expected {
			d, p := r.locate(int64(unit))
			assert.Equal(t, want, [2]int{d, p}, "layout %d unit %d", tt.layout, unit)
		}
	}
}

// buildArray splits a logical image into n members with parity, using the locate rules of a
// reference RAID5 so the members are laid out exactly as a real array would be.
func buildArray(t *testing.T, image []byte, n int, layout Layout) []Source {
	t.Helper()
	require.Equal(t, 0, len(image)%(raidStripe*(n-1)))
	rows := len(image) / (raidStripe * (n - 1))
	disks := make([][]byte, n)
	for i := range disks {
		disks[i] = make([]byte, rows*raidStripe)
	}
	ref := &RAID5{members: make([]Source, n), stripeSize: raidStripe, layout: layout}
	for unit := 0; unit < rows*(n-1); unit++ {
		d, p := ref.locate(int64(unit))
		row := unit / (n - 1)
		data := image[unit*raidStripe : (unit+1)*raidStripe]
		copy(disks[d][row*raidStripe:], data)
		parity := disks[p][row*raidStripe : (row+1)*raidStripe]
		for i := range data {
			parity[i] ^= data[i]
		}
	}
	members := make([]Source, n)
	for i := range members {
		members[i] = &mem{data: disks[i]}
	}
	return members
}

func TestRAID5Read(t *testing.T) {
	t.Parallel()
	for _, layout := range []Layout{LeftSymmetric, LeftAsymmetric, RightSymmetric, RightAsymmetric} {
		for _, n := range []int{3, 4, 5} {
			image := pattern(6 * raidStripe * (n - 1)) // 6 rows
			members := buildArray(t, image, n, layout)
			r, err := NewRAID5(members, raidStripe, layout, 0)
			require.NoError(t, err)
			assert.Equal(t, int64(len(image)), r.Size())
			got := make([]byte, len(image))
			nread, err := r.ReadAt(got, 0)
			require.NoError(t, err)
			assert.Equal(t, len(image), nread)
			require.True(t, bytes.Equal(image, got), "layout %d n %d: full read differs", layout, n)
			// Unaligned windows across unit and row boundaries
			for off := 0; off < len(image); off += 777 {
				size := min(2*raidStripe+333, len(image)-off)
				p := make([]byte, size)
				nread, err := r.ReadAt(p, int64(off))
				require.NoError(t, err)
				assert.Equal(t, size, nread)
				require.True(t, bytes.Equal(image[off:off+size], p), "layout %d n %d off %d", layout, n, off)
			}
			require.NoError(t, r.Close())
		}
	}
}

func TestRAID5Degraded(t *testing.T) {
	t.Parallel()
	n := 4
	image := pattern(5 * raidStripe * (n - 1))
	for missing := 0; missing < n; missing++ {
		members := buildArray(t, image, n, LeftSymmetric)
		members[missing] = nil
		r, err := NewRAID5(members, raidStripe, LeftSymmetric, 0)
		require.NoError(t, err)
		got := make([]byte, len(image))
		_, err = r.ReadAt(got, 0)
		require.NoError(t, err)
		require.True(t, bytes.Equal(image, got), "missing member %d", missing)
		p := make([]byte, 500)
		_, err = r.ReadAt(p, 3*raidStripe-100)
		require.NoError(t, err)
		assert.Equal(t, image[3*raidStripe-100:3*raidStripe+400], p)
	}
}

func TestRAID5SizeAndEOF(t *testing.T) {
	t.Parallel()
	image := pattern(4 * raidStripe * 2)
	members := buildArray(t, image, 3, LeftSymmetric)
	// Explicit smaller size
	r, err := NewRAID5(members, raidStripe, LeftSymmetric, 3*raidStripe)
	require.NoError(t, err)
	assert.Equal(t, int64(3*raidStripe), r.Size())
	p := make([]byte, 100)
	nread, err := r.ReadAt(p, 3*raidStripe-50)
	assert.ErrorIs(t, err, io.EOF)
	assert.Equal(t, 50, nread)
	assert.Equal(t, image[3*raidStripe-50:3*raidStripe], p[:50])
	// Members of unequal length: capacity follows the smallest, rounded to whole rows
	members[1] = &mem{data: members[1].(*mem).data[:raidStripe+100]}
	r, err = NewRAID5(members, raidStripe, LeftSymmetric, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(2*raidStripe), r.Size())
}

func TestRAID5Errors(t *testing.T) {
	t.Parallel()
	image := pattern(2 * raidStripe * 2)
	members := buildArray(t, image, 3, LeftSymmetric)
	_, err := NewRAID5(members[:2], raidStripe, LeftSymmetric, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least 3")
	members[0], members[1] = nil, nil
	_, err = NewRAID5(members, raidStripe, LeftSymmetric, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "one missing")
	members = buildArray(t, image, 3, LeftSymmetric)
	_, err = NewRAID5(members, raidStripe, LeftSymmetric, 5*raidStripe)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")
	_, err = NewRAID5(members, raidStripe, LeftSymmetric, 100)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple of the stripe size")
}

func TestRAID5Direct(t *testing.T) {
	t.Parallel()
	image := pattern(2 * raidStripe * 2)
	members := buildArray(t, image, 3, LeftSymmetric)
	// Wrap member 0 in a cache whose fast tier is wrong, so only direct reads are right
	wrong := &tier{mem: mem{data: filled(len(members[0].(*mem).data), 'X').data}}
	members[0] = NewCache(wrong, members[0])
	r, err := NewRAID5(members, raidStripe, LeftSymmetric, 0)
	require.NoError(t, err)
	got := make([]byte, len(image))
	_, err = ReadDirect(r, got, 0)
	require.NoError(t, err)
	assert.Equal(t, image, got)
	_, err = r.ReadAt(got, 0)
	require.NoError(t, err)
	assert.NotEqual(t, image, got)
}
