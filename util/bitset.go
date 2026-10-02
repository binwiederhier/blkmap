package util

// Bitset is a fixed-size set of chunk indices, 1 bit each (a 1 TiB device at 64 KiB chunks
// needs 2 MiB), for bookkeeping that would otherwise be a []bool or a map.
type Bitset struct {
	words []uint64
	n     int64
}

func NewBitset(n int64) *Bitset {
	return &Bitset{words: make([]uint64, (n+63)/64), n: n}
}

func (b *Bitset) Set(i int64) {
	b.words[i/64] |= 1 << (i % 64)
}

func (b *Bitset) Test(i int64) bool {
	return b.words[i/64]&(1<<(i%64)) != 0
}

// Len returns the number of bits.
func (b *Bitset) Len() int64 {
	return b.n
}
