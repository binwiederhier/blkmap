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
		s, err := parseSegment(i, rs)
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

func parseSegment(i int, raw *rawSegment) (*Segment, error) {
	s := &Segment{Type: SourceType(raw.Type), Offset: -1, Path: raw.Path, URL: raw.URL}
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
	switch s.Type {
	case SourceZero:
		if s.Size == 0 {
			return nil, fmt.Errorf("%w: segment %d: zero segment needs a size", errConfig, i)
		}
	case SourceFile, SourceDevice:
		if s.Path == "" {
			return nil, fmt.Errorf("%w: segment %d: %s segment needs a path", errConfig, i, s.Type)
		}
		if s.URL != "" {
			return nil, fmt.Errorf("%w: segment %d: url is only valid for http segments", errConfig, i)
		}
	case SourceHTTP:
		if s.URL == "" {
			return nil, fmt.Errorf("%w: segment %d: http segment needs a url", errConfig, i)
		}
		if s.Path != "" {
			return nil, fmt.Errorf("%w: segment %d: path is only valid for file and device segments", errConfig, i)
		}
	default:
		return nil, fmt.Errorf("%w: segment %d: unknown segment type %q", errConfig, i, raw.Type)
	}
	return s, nil
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
