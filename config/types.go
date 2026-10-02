package config

import "time"

// SourceType identifies what backs a segment of the device.
type SourceType string

const (
	SourceZero   SourceType = "zero"
	SourceFile   SourceType = "file"
	SourceDevice SourceType = "device"
	SourceHTTP   SourceType = "http"
	SourceRAID5  SourceType = "raid5"
	SourceCache  SourceType = "cache"
	SourceCustom SourceType = "custom"
)

// Hydration cache policies: whether background reads go through cache tiers.
const (
	CacheAlways = "always"
	CacheNever  = "never"
)

// RAID-5 parity rotation layouts, named as Linux md does. Windows dynamic disks (LDM) use
// left-symmetric, which is also md's default.
const (
	LayoutLeftSymmetric   = "left-symmetric"
	LayoutLeftAsymmetric  = "left-asymmetric"
	LayoutRightSymmetric  = "right-symmetric"
	LayoutRightAsymmetric = "right-asymmetric"
)

// Config is the resolved, validated configuration of one blkmap device.
type Config struct {
	ID        string
	Size      int64 // 0 means "end of the last segment"
	BlockSize int
	ReadOnly  bool
	COW       *COW
	Segments  []*Segment
	Hydrate   *Hydrate // nil: no background hydration
}

// Hydrate configures background copying of the base into the COW file.
type Hydrate struct {
	PrefetchList string        // file of ranges to hydrate first, highest priority first
	Rest         bool          // hydrate everything not listed, after the list
	Rate         int64         // bytes/s cap for the rest phase; 0 = unlimited
	UseCache     string        // CacheAlways or CacheNever
	Concurrency  int           // parallel background reads
	ReportEvery  time.Duration // progress log interval
}

// COW describes where writes are stored.
type COW struct {
	File      string
	Bitmap    string
	ChunkSize int64
}

// Segment is one piece of the device address space, or a nested source (a raid5 member, a
// cache tier), in which case Offset is unused. Offset < 0 means "directly after the previous
// segment"; Size 0 means "the remaining length of the source after SourceOffset".
type Segment struct {
	Type         SourceType
	Offset       int64
	Size         int64
	Path         string
	URL          string
	SourceOffset int64

	// raid5 only: the array geometry and its members in array order; a member may be Missing
	StripeSize int64
	Layout     string
	Members    []*Segment
	Missing    bool

	// cache only: the tiers
	Fast *Segment
	Slow *Segment

	// custom only: the registered constructor and its parameters
	Name   string
	Params map[string]string

	// Map is a path or URL of a data-extent map for this source (any type); everything
	// not listed reads as zeros and is never fetched.
	Map string
}

// rawConfig mirrors the YAML file; sizes are strings so they can carry K/M/G suffixes.
type rawConfig struct {
	Size      string        `yaml:"size"`
	BlockSize int           `yaml:"block-size"`
	ReadOnly  bool          `yaml:"read-only"`
	COW       *rawCOW       `yaml:"cow"`
	Segments  []*rawSegment `yaml:"segments"`
	Hydrate   *rawHydrate   `yaml:"hydrate"`
}

type rawHydrate struct {
	PrefetchList string `yaml:"prefetch-list"`
	Rest         *bool  `yaml:"rest"`
	Rate         string `yaml:"rate"`
	UseCache     string `yaml:"use-cache"`
	Concurrency  int    `yaml:"concurrency"`
	ReportEvery  string `yaml:"report-every"`
}

type rawCOW struct {
	File      string `yaml:"file"`
	Bitmap    string `yaml:"bitmap"`
	ChunkSize string `yaml:"chunk-size"`
}

// rawSegment is a top-level segment or a nested source; offset is rejected when nested.
type rawSegment struct {
	Type         string            `yaml:"type"`
	Offset       string            `yaml:"offset"`
	Size         string            `yaml:"size"`
	Path         string            `yaml:"path"`
	URL          string            `yaml:"url"`
	SourceOffset string            `yaml:"source-offset"`
	StripeSize   string            `yaml:"stripe-size"`
	Layout       string            `yaml:"layout"`
	Members      []*rawSegment     `yaml:"members"`
	Missing      bool              `yaml:"missing"`
	Fast         *rawSegment       `yaml:"fast"`
	Slow         *rawSegment       `yaml:"slow"`
	Name         string            `yaml:"name"`
	Params       map[string]string `yaml:"params"`
	Map          string            `yaml:"map"`
}
