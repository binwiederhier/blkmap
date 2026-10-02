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

// Holes reports the whole requested range.
func (z *Zero) Holes(off, length int64) ([]Range, error) {
	if off < 0 || off >= z.size || length <= 0 {
		return nil, nil
	}
	return []Range{{Offset: off, Length: min(length, z.size-off)}}, nil
}

func (z *Zero) Close() error {
	return nil
}
