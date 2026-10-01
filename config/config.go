// Package config loads and validates /etc/blkmap/<id>.yml device definitions.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

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
	// cowExt and bitmapExt name the default state files, <id>.cow and <id>.cow.bitmap.
	cowExt    = ".cow"
	bitmapExt = ".bitmap"
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
		s, err := parseSegment(i, rs, c.BlockSize)
		if err != nil {
			return nil, err
		}
		c.Segments = append(c.Segments, s)
	}
	if err := c.checkAlignment(); err != nil {
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
	if err := dec.Decode(raw); err != nil && !errors.Is(err, errEOF(err)) {
		return nil, err
	}
	return raw, nil
}

// errEOF returns err itself when it is the decoder's end-of-input (empty file), else nil,
// so an empty config decodes to the zero rawConfig and fails validation with a clear message.
func errEOF(err error) error {
	if err != nil && err.Error() == "EOF" {
		return err
	}
	return nil
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
		cow.Bitmap = cow.File + bitmapExt
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

func parseSegment(i int, raw *rawSegment, blockSize int) (*Segment, error) {
	s := &Segment{Type: SourceType(raw.Type), Offset: -1, Path: raw.Path, URL: raw.URL, Layout: raw.Layout}
	var err error
	if raw.Offset != "" {
		if s.Offset, err = parseSize(fmt.Sprintf("segment %d offset", i), raw.Offset, 0); err != nil {
			return nil, err
		}
	}
	if s.Size, err = parseSize(fmt.Sprintf("segment %d size", i), raw.Size, 0); err != nil {
		return nil, err
	}
	if s.SourceOffset, err = parseSize(fmt.Sprintf("segment %d source-offset", i), raw.SourceOffset, 0); err != nil {
		return nil, err
	}
	if s.Type != SourceRAID5 && (len(raw.Members) > 0 || raw.StripeSize != "" || raw.Layout != "") {
		return nil, fmt.Errorf("%w: segment %d: members, stripe-size and layout are only valid for raid5 segments", errConfig, i)
	}
	if s.Type != SourceFile && s.Type != SourceDevice && s.Path != "" {
		return nil, fmt.Errorf("%w: segment %d: path is only valid for file and device segments", errConfig, i)
	}
	if s.Type != SourceHTTP && s.URL != "" {
		return nil, fmt.Errorf("%w: segment %d: url is only valid for http segments", errConfig, i)
	}
	switch s.Type {
	case SourceZero:
		if s.Size == 0 {
			return nil, fmt.Errorf("%w: segment %d: zero segment needs a size", errConfig, i)
		}
	case SourceFile, SourceDevice:
		if s.Path == "" {
			return nil, fmt.Errorf("%w: segment %d: %s segment needs a path", errConfig, i, s.Type)
		}
	case SourceHTTP:
		if s.URL == "" {
			return nil, fmt.Errorf("%w: segment %d: http segment needs a url", errConfig, i)
		}
	case SourceRAID5:
		if err := parseRAID5(i, raw, s, blockSize); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w: segment %d: unknown segment type %q", errConfig, i, raw.Type)
	}
	return s, nil
}

// parseRAID5 fills in the array geometry and members of a raid5 segment.
func parseRAID5(i int, raw *rawSegment, s *Segment, blockSize int) error {
	var err error
	if s.StripeSize, err = parseSize(fmt.Sprintf("segment %d stripe-size", i), raw.StripeSize, DefaultStripeSize); err != nil {
		return err
	}
	if s.StripeSize&(s.StripeSize-1) != 0 {
		return fmt.Errorf("%w: segment %d: stripe-size must be a power of two", errConfig, i)
	}
	if s.StripeSize < int64(blockSize) {
		return fmt.Errorf("%w: segment %d: stripe-size must be at least the block size (%d)", errConfig, i, blockSize)
	}
	if s.Layout == "" {
		s.Layout = LayoutLeftSymmetric
	}
	if !layouts[s.Layout] {
		return fmt.Errorf("%w: segment %d: unknown layout %q", errConfig, i, s.Layout)
	}
	if len(raw.Members) < 3 {
		return fmt.Errorf("%w: segment %d: raid5 needs at least 3 members", errConfig, i)
	}
	missing := 0
	for j, rm := range raw.Members {
		m, err := parseMember(i, j, rm)
		if err != nil {
			return err
		}
		if m.Missing {
			missing++
		}
		s.Members = append(s.Members, m)
	}
	if missing > 1 {
		return fmt.Errorf("%w: segment %d: at most one member can be missing", errConfig, i)
	}
	return nil
}

func parseMember(i, j int, raw *rawMember) (*Member, error) {
	m := &Member{Type: SourceType(raw.Type), Path: raw.Path, URL: raw.URL, Missing: raw.Missing}
	var err error
	if m.SourceOffset, err = parseSize(fmt.Sprintf("segment %d member %d source-offset", i, j), raw.SourceOffset, 0); err != nil {
		return nil, err
	}
	if m.Size, err = parseSize(fmt.Sprintf("segment %d member %d size", i, j), raw.Size, 0); err != nil {
		return nil, err
	}
	if m.Missing {
		if raw.Type != "" || m.Path != "" || m.URL != "" {
			return nil, fmt.Errorf("%w: segment %d member %d: a missing member has no type, path or url", errConfig, i, j)
		}
		return m, nil
	}
	switch m.Type {
	case SourceFile, SourceDevice:
		if m.Path == "" {
			return nil, fmt.Errorf("%w: segment %d member %d: %s member needs a path", errConfig, i, j, m.Type)
		}
	case SourceHTTP:
		if m.URL == "" {
			return nil, fmt.Errorf("%w: segment %d member %d: http member needs a url", errConfig, i, j)
		}
	default:
		return nil, fmt.Errorf("%w: segment %d member %d: unknown member type %q", errConfig, i, j, raw.Type)
	}
	return m, nil
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
)
