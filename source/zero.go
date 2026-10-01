package source

// Zero reads as all zeros.
type Zero struct {
	size int64
}

// NewZero returns a zero-filled source of the given size.
func NewZero(size int64) *Zero {
	return &Zero{size: size}
}

func (z *Zero) ReadAt(p []byte, off int64) (int, error) {
	n, err := clampRead(len(p), off, z.size)
	clear(p[:n])
	return n, err
}

func (z *Zero) Size() int64 {
	return z.size
}

// ZeroRanges reports the whole source.
func (z *Zero) ZeroRanges() []Range {
	return []Range{{Offset: 0, Length: z.size}}
}

func (z *Zero) Close() error {
	return nil
}
