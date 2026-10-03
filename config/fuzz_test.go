package config

import (
	"testing"
)

// FuzzParse feeds arbitrary YAML to the config parser: it must reject or accept, never
// panic, and anything it accepts must satisfy its own invariants.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"segments:\n  - type: zero\n    size: 1M\n",
		"size: 2M\nblock-size: 4096\ncow:\n  file: /x.cow\n  chunk-size: 128K\nsegments:\n  - type: file\n    path: /dev/null\n    offset: 1M\n",
		"segments:\n  - type: raid5\n    stripe-size: 64K\n    members:\n      - type: zero\n        size: 1M\n      - missing: true\n      - type: zero\n        size: 1M\n",
		"segments:\n  - type: cache\n    fast: {type: zero, size: 1M}\n    slow: {type: zero, size: 1M}\nhydrate:\n  rate: 1M\n  rest: false\n",
		"segments: []\n", "", "garbage: [", "size: -1\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, content string) {
		c, err := Parse("fuzz", []byte(content))
		if err != nil {
			return
		}
		// Size 0 is allowed at parse time (it is the sum of the segments, known once they open)
		if c.Size < 0 || len(c.Segments) == 0 || c.BlockSize <= 0 || c.COW.ChunkSize <= 0 {
			t.Fatalf("accepted config violates invariants: %+v", c)
		}
	})
}
