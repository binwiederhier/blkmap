package config

// SourceType identifies what backs a segment of the device.
type SourceType string

const (
	SourceZero   SourceType = "zero"
	SourceFile   SourceType = "file"
	SourceDevice SourceType = "device"
	SourceHTTP   SourceType = "http"
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
	Type         string `yaml:"type"`
	Offset       string `yaml:"offset"`
	Size         string `yaml:"size"`
	Path         string `yaml:"path"`
	URL          string `yaml:"url"`
	SourceOffset string `yaml:"source-offset"`
}
