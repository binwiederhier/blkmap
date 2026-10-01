package config

// SourceType identifies what backs a segment of the device.
type SourceType string

const (
	SourceZero   SourceType = "zero"
	SourceFile   SourceType = "file"
	SourceDevice SourceType = "device"
	SourceHTTP   SourceType = "http"
	SourceRAID5  SourceType = "raid5"
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
}

// COW describes where writes are stored.
type COW struct {
	File      string
	Bitmap    string
	ChunkSize int64
}

// Segment is one piece of the device address space. Offset < 0 means "directly after the
// previous segment"; Size 0 means "the remaining length of the source after SourceOffset".
type Segment struct {
	Type         SourceType
	Offset       int64
	Size         int64
	Path         string
	URL          string
	SourceOffset int64

	// raid5 only: the array geometry and its members in array order
	StripeSize int64
	Layout     string
	Members    []*Member
}

// Member is one disk of a raid5 segment: a file, device or http source, or a missing disk
// that is reconstructed from parity.
type Member struct {
	Type         SourceType
	Path         string
	URL          string
	SourceOffset int64
	Size         int64
	Missing      bool
}

// rawConfig mirrors the YAML file; sizes are strings so they can carry K/M/G suffixes.
type rawConfig struct {
	Size      string        `yaml:"size"`
	BlockSize int           `yaml:"block-size"`
	ReadOnly  bool          `yaml:"read-only"`
	COW       *rawCOW       `yaml:"cow"`
	Segments  []*rawSegment `yaml:"segments"`
}

type rawCOW struct {
	File      string `yaml:"file"`
	Bitmap    string `yaml:"bitmap"`
	ChunkSize string `yaml:"chunk-size"`
}

type rawSegment struct {
	Type         string       `yaml:"type"`
	Offset       string       `yaml:"offset"`
	Size         string       `yaml:"size"`
	Path         string       `yaml:"path"`
	URL          string       `yaml:"url"`
	SourceOffset string       `yaml:"source-offset"`
	StripeSize   string       `yaml:"stripe-size"`
	Layout       string       `yaml:"layout"`
	Members      []*rawMember `yaml:"members"`
}

type rawMember struct {
	Type         string `yaml:"type"`
	Path         string `yaml:"path"`
	URL          string `yaml:"url"`
	SourceOffset string `yaml:"source-offset"`
	Size         string `yaml:"size"`
	Missing      bool   `yaml:"missing"`
}
