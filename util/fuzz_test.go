package util

import (
	"testing"
)

func FuzzParseSize(f *testing.F) {
	for _, seed := range []string{"0", "1", "64K", "1.5G", "16T", "-1", "", "K", "99999999999999999999", "1e9", "0x10"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		n, err := ParseSize(s)
		if err == nil && n < 0 {
			t.Fatalf("ParseSize(%q) = %d, negative without error", s, n)
		}
	})
}
