// Package util holds small helpers shared across blkmap packages.
package util

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	sizeUnitK = 1 << 10
	sizeUnitM = 1 << 20
	sizeUnitG = 1 << 30
	sizeUnitT = 1 << 40
)

var (
	sizeSuffixes = map[string]int64{
		"":  1,
		"k": sizeUnitK,
		"m": sizeUnitM,
		"g": sizeUnitG,
		"t": sizeUnitT,
	}
	// Largest unit first so FormatSize picks the biggest exact one
	sizeFormatUnits = []struct {
		unit   int64
		suffix string
	}{
		{sizeUnitT, "T"},
		{sizeUnitG, "G"},
		{sizeUnitM, "M"},
		{sizeUnitK, "K"},
	}
	errInvalidSize = errors.New("invalid size")
)

// ParseSize parses a byte size with an optional binary suffix (K, M, G, T, also Ki/KiB forms).
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("%w: empty", errInvalidSize)
	}
	// Split into digits and suffix; the suffix is case-insensitive and may carry "i"/"iB"
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	digits, suffix := s[:i], strings.ToLower(s[i:])
	suffix = strings.TrimSuffix(strings.TrimSuffix(suffix, "b"), "i")
	mult, ok := sizeSuffixes[suffix]
	if !ok || digits == "" {
		return 0, fmt.Errorf("%w: %q", errInvalidSize, s)
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q: %w", errInvalidSize, s, err)
	}
	if n > 0 && n > (1<<63-1)/mult {
		return 0, fmt.Errorf("%w: %q overflows", errInvalidSize, s)
	}
	return n * mult, nil
}

// FormatSize renders a byte count with the largest exact binary suffix, e.g. 65536 -> "64K".
func FormatSize(n int64) string {
	for _, u := range sizeFormatUnits {
		if n != 0 && n%u.unit == 0 {
			return fmt.Sprintf("%d%s", n/u.unit, u.suffix)
		}
	}
	return strconv.FormatInt(n, 10)
}
