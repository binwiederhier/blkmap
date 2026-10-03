// Package config loads and validates /etc/blkmap/<id>.yml device definitions.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"

	"heckel.io/blkmap/util"
)

const (
	// DefaultDir is where "blkmap serve <id>" looks for <id>.yml.
	DefaultDir = "/etc/blkmap"
	// DefaultStateDir holds the COW and bitmap files unless the config says otherwise.
	DefaultStateDir = "/var/lib/blkmap"
	// DefaultBlockSize is the logical block size the kernel sees.
	DefaultBlockSize = 512
	// DefaultChunkSize is the COW bitmap granularity.
	DefaultChunkSize = 64 << 10
	// configExt is the config file extension inside DefaultDir.
	configExt = ".yml"
	// cowExt and BitmapExt name the default state files, <id>.cow and <id>.cow.bitmap.
	cowExt    = ".cow"
	BitmapExt = ".bitmap"
	// blockSizeLarge is the only other logical block size the kernel accepts on x86 (page size).
	blockSizeLarge = 4096
)

var (
	// idRegex keeps ids usable as file names, systemd instance names, and /dev/blkmap entries.
	idRegex   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	errConfig = errors.New("invalid config")
	layouts   = map[string]bool{
		LayoutLeftSymmetric:   true,
		LayoutLeftAsymmetric:  true,
		LayoutRightSymmetric:  true,
		LayoutRightAsymmetric: true,
	}
)

// Path returns the config file path for a device id in dir.
func Path(dir, id string) string {
	return filepath.Join(dir, id+configExt)
}

// Load reads and validates the config file for the device id at path.
func Load(id, path string) (*Config, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(id, content)
}

// Parse parses and validates YAML config content for the device id.
func Parse(id string, content []byte) (*Config, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("%w: invalid device id %q", errConfig, id)
	}
	raw, err := decode(content)
	if err != nil {
		return nil, fmt.Errorf("%w: yaml: %w", errConfig, err)
	}
	c := &Config{
		ID:        id,
		BlockSize: DefaultBlockSize,
		ReadOnly:  raw.ReadOnly,
	}
	if raw.BlockSize != 0 {
		c.BlockSize = raw.BlockSize
	}
	if c.BlockSize != DefaultBlockSize && c.BlockSize != blockSizeLarge {
		return nil, fmt.Errorf("%w: block-size must be %d or %d", errConfig, DefaultBlockSize, blockSizeLarge)
	}
	if c.Size, err = parseSize("size", raw.Size, 0); err != nil {
		return nil, err
	}
	if c.COW, err = parseCOW(id, raw.COW, c.BlockSize); err != nil {
		return nil, err
	}
	if len(raw.Segments) == 0 {
		return nil, fmt.Errorf("%w: at least one segment is required", errConfig)
	}
	for i, rs := range raw.Segments {
		s, err := parseSegment(fmt.Sprintf("segment %d", i), rs, c.BlockSize, false)
		if err != nil {
			return nil, err
		}
		c.Segments = append(c.Segments, s)
	}
	if err := c.checkAlignment(); err != nil {
		return nil, err
	}
	if c.Hydrate, err = parseHydrate(raw.Hydrate); err != nil {
		return nil, err
	}
	if c.Record, err = parseRecord(raw.Record); err != nil {
		return nil, err
	}
	return c, nil
}

// ValidID reports whether id is safe to use in device and file names.
func ValidID(id string) bool {
	return idRegex.MatchString(id)
}

// checkAlignment rejects sizes and offsets the kernel could not address in whole blocks.
func (c *Config) checkAlignment() error {
	bs := int64(c.BlockSize)
	if c.Size%bs != 0 {
		return fmt.Errorf("%w: size must be a multiple of block-size (%d)", errConfig, bs)
	}
	for i, s := range c.Segments {
		if s.Size%bs != 0 || (s.Offset > 0 && s.Offset%bs != 0) {
			return fmt.Errorf("%w: segment %d: offset and size must be a multiple of block-size (%d)", errConfig, i, bs)
		}
	}
	return nil
}

// decode unmarshals strictly so a misspelled key is an error rather than silently ignored.
func decode(content []byte) (*rawConfig, error) {
	raw := &rawConfig{}
	dec := yaml.NewDecoder(bytes.NewReader(content))
	dec.KnownFields(true)
	if err := dec.Decode(raw); err != nil && !errors.Is(err, io.EOF) { // empty file: zero config, fails validation
		return nil, err
	}
	return raw, nil
}

func parseCOW(id string, raw *rawCOW, blockSize int) (*COW, error) {
	if raw == nil {
		raw = &rawCOW{}
	}
	cow := &COW{File: raw.File, Bitmap: raw.Bitmap}
	if cow.File == "" {
		cow.File = filepath.Join(DefaultStateDir, id+cowExt)
	}
	if cow.Bitmap == "" {
		cow.Bitmap = cow.File + BitmapExt
	}
	var err error
	if cow.ChunkSize, err = parseSize("cow.chunk-size", raw.ChunkSize, DefaultChunkSize); err != nil {
		return nil, err
	}
	if cow.ChunkSize&(cow.ChunkSize-1) != 0 {
		return nil, fmt.Errorf("%w: cow.chunk-size must be a power of two", errConfig)
	}
	if cow.ChunkSize < int64(blockSize) {
		return nil, fmt.Errorf("%w: cow.chunk-size must be at least the block size (%d)", errConfig, blockSize)
	}
	return cow, nil
}

// parseSegment parses a top-level segment, or a nested source (raid5 member, cache tier) when
// nested is set; where is the position used in error messages.
func parseSegment(where string, raw *rawSegment, blockSize int, nested bool) (*Segment, error) {
	if raw == nil {
		return nil, fmt.Errorf("%w: %s is empty", errConfig, where)
	}
	s := &Segment{Type: SourceType(raw.Type), Offset: -1, Path: raw.Path, URL: raw.URL, Layout: raw.Layout, Missing: raw.Missing, Name: raw.Name, Params: raw.Params, Map: raw.Map}
	var err error
	if raw.Offset != "" {
		if nested {
			return nil, fmt.Errorf("%w: %s: offset is only valid for top-level segments", errConfig, where)
		}
		if s.Offset, err = parseSize(where+" offset", raw.Offset, 0); err != nil {
			return nil, err
		}
	}
	if s.Size, err = parseSize(where+" size", raw.Size, 0); err != nil {
		return nil, err
	}
	if s.SourceOffset, err = parseSize(where+" source-offset", raw.SourceOffset, 0); err != nil {
		return nil, err
	}
	if s.Missing {
		if !nested {
			return nil, fmt.Errorf("%w: %s: missing is only valid for raid5 members", errConfig, where)
		}
		if raw.Type != "" || s.Path != "" || s.URL != "" {
			return nil, fmt.Errorf("%w: %s: a missing member has no type, path or url", errConfig, where)
		}
		return s, nil
	}
	// Fields that belong to one type only
	if s.Type != SourceRAID5 && (len(raw.Members) > 0 || raw.StripeSize != "" || raw.Layout != "") {
		return nil, fmt.Errorf("%w: %s: members, stripe-size and layout are only valid for raid5 segments", errConfig, where)
	}
	if s.Type != SourceCache && (raw.Fast != nil || raw.Slow != nil) {
		return nil, fmt.Errorf("%w: %s: fast and slow are only valid for cache segments", errConfig, where)
	}
	if s.Type != SourceCustom && (raw.Name != "" || len(raw.Params) > 0) {
		return nil, fmt.Errorf("%w: %s: name and params are only valid for custom segments", errConfig, where)
	}
	// A map makes sense for sources that are opaque about holes; zero and raid5 are not
	if s.Map != "" && (s.Type == SourceZero || s.Type == SourceRAID5 || s.Type == SourceCache) {
		return nil, fmt.Errorf("%w: %s: map is not valid for %s segments (put it on the tiers or members)", errConfig, where, s.Type)
	}
	if s.Type != SourceFile && s.Type != SourceDevice && s.Path != "" {
		return nil, fmt.Errorf("%w: %s: path is only valid for file and device segments", errConfig, where)
	}
	if s.Type != SourceHTTP && s.URL != "" {
		return nil, fmt.Errorf("%w: %s: url is only valid for http segments", errConfig, where)
	}
	switch s.Type {
	case SourceZero:
		if s.Size == 0 {
			return nil, fmt.Errorf("%w: %s: zero segment needs a size", errConfig, where)
		}
	case SourceFile, SourceDevice:
		if s.Path == "" {
			return nil, fmt.Errorf("%w: %s: %s segment needs a path", errConfig, where, s.Type)
		}
	case SourceHTTP:
		if s.URL == "" {
			return nil, fmt.Errorf("%w: %s: http segment needs a url", errConfig, where)
		}
	case SourceRAID5:
		if err := parseRAID5(where, raw, s, blockSize); err != nil {
			return nil, err
		}
	case SourceCache:
		if raw.Fast == nil || raw.Slow == nil {
			return nil, fmt.Errorf("%w: %s: cache segment needs fast and slow sources", errConfig, where)
		}
		if s.Fast, err = parseSegment(where+": fast", raw.Fast, blockSize, true); err != nil {
			return nil, err
		}
		if s.Slow, err = parseSegment(where+": slow", raw.Slow, blockSize, true); err != nil {
			return nil, err
		}
	case SourceCustom:
		if s.Name == "" {
			return nil, fmt.Errorf("%w: %s: custom segment needs a name", errConfig, where)
		}
	default:
		return nil, fmt.Errorf("%w: %s: unknown segment type %q", errConfig, where, raw.Type)
	}
	return s, nil
}

// parseRAID5 fills in the array geometry and members of a raid5 segment.
func parseRAID5(where string, raw *rawSegment, s *Segment, blockSize int) error {
	var err error
	if s.StripeSize, err = parseSize(where+" stripe-size", raw.StripeSize, DefaultStripeSize); err != nil {
		return err
	}
	if s.StripeSize&(s.StripeSize-1) != 0 {
		return fmt.Errorf("%w: %s: stripe-size must be a power of two", errConfig, where)
	}
	if s.StripeSize < int64(blockSize) {
		return fmt.Errorf("%w: %s: stripe-size must be at least the block size (%d)", errConfig, where, blockSize)
	}
	if s.Layout == "" {
		s.Layout = LayoutLeftSymmetric
	}
	if !layouts[s.Layout] {
		return fmt.Errorf("%w: %s: unknown layout %q", errConfig, where, s.Layout)
	}
	if len(raw.Members) < 3 {
		return fmt.Errorf("%w: %s: raid5 needs at least 3 members", errConfig, where)
	}
	missing := 0
	for j, rm := range raw.Members {
		m, err := parseSegment(fmt.Sprintf("%s member %d", where, j), rm, blockSize, true)
		if err != nil {
			return err
		}
		if m.Missing {
			missing++
		}
		s.Members = append(s.Members, m)
	}
	if missing > 1 {
		return fmt.Errorf("%w: %s: at most one member can be missing", errConfig, where)
	}
	return nil
}

// parseHydrate applies the defaults and validates the hydrate block; nil means off.
func parseHydrate(raw *rawHydrate) (*Hydrate, error) {
	if raw == nil {
		return nil, nil
	}
	h := &Hydrate{PrefetchList: raw.PrefetchList, Rest: true, UseCache: CacheAlways, Concurrency: DefaultHydrateConcurrency, ReportEvery: DefaultHydrateReport}
	if raw.Rest != nil {
		h.Rest = *raw.Rest
	}
	var err error
	if h.Rate, err = parseSize("hydrate.rate", raw.Rate, 0); err != nil {
		return nil, err
	}
	if raw.UseCache != "" {
		h.UseCache = raw.UseCache
	}
	if h.UseCache != CacheAlways && h.UseCache != CacheNever {
		return nil, fmt.Errorf("%w: hydrate.use-cache must be %s or %s", errConfig, CacheAlways, CacheNever)
	}
	if raw.Concurrency != 0 {
		h.Concurrency = raw.Concurrency
	}
	if h.Concurrency < 1 {
		return nil, fmt.Errorf("%w: hydrate.concurrency must be at least 1", errConfig)
	}
	if raw.ReportEvery != "" {
		if h.ReportEvery, err = time.ParseDuration(raw.ReportEvery); err != nil {
			return nil, fmt.Errorf("%w: hydrate.report-every: %w", errConfig, err)
		}
	}
	if h.ReportEvery <= 0 {
		return nil, fmt.Errorf("%w: hydrate.report-every must be positive", errConfig)
	}
	return h, nil
}

// parseRecord applies the defaults and validates the record block; nil means off.
func parseRecord(raw *rawRecord) (*Record, error) {
	if raw == nil {
		return nil, nil
	}
	if raw.File == "" {
		return nil, fmt.Errorf("%w: record.file is required", errConfig)
	}
	r := &Record{File: raw.File}
	if raw.MaxDuration != "" {
		d, err := time.ParseDuration(raw.MaxDuration)
		if err != nil {
			return nil, fmt.Errorf("%w: record.max-duration: %w", errConfig, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("%w: record.max-duration must be positive", errConfig)
		}
		r.MaxDuration = d
	}
	var err error
	if r.MaxSize, err = parseSize("record.max-size", raw.MaxSize, DefaultRecordMaxSize); err != nil {
		return nil, err
	}
	if r.MaxSize <= 0 {
		return nil, fmt.Errorf("%w: record.max-size must be positive", errConfig)
	}
	return r, nil
}

// parseSize parses an optional size field, falling back to def when the field is empty.
func parseSize(field, value string, def int64) (int64, error) {
	if value == "" {
		return def, nil
	}
	n, err := util.ParseSize(value)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: %w", errConfig, field, err)
	}
	return n, nil
}

const (
	// DefaultStripeSize is the stripe unit of a raid5 segment; 64 KiB is the Windows default.
	DefaultStripeSize = 64 << 10
	// DefaultHydrateConcurrency is the number of parallel background hydration reads.
	DefaultHydrateConcurrency = 4
	// DefaultHydrateReport is how often hydration progress is logged.
	DefaultHydrateReport = 30 * time.Second
	// DefaultRecordMaxSize bounds a recording file.
	DefaultRecordMaxSize = 16 << 20
)
