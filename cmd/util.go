package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"heckel.io/blkmap/config"
)

var (
	errNoID = errors.New("device ID or FILE argument required")
)

// loadConfig resolves an "ID|FILE" argument. With allowPath, anything that looks like a path
// (contains a separator, ends in .yml, or exists as a file) is read directly and the ID is
// its base name; otherwise the ID is looked up in /etc/blkmap. An explicit path overrides both.
func loadConfig(arg, explicitPath string, allowPath bool) (*config.Config, error) {
	if arg == "" {
		return nil, errNoID
	}
	path := explicitPath
	id := arg
	if path == "" && allowPath && isPath(arg) {
		path = arg
		id = strings.TrimSuffix(filepath.Base(arg), filepath.Ext(arg))
	} else if path == "" {
		path = config.Path(config.DefaultDir, id)
	}
	if !config.ValidID(id) {
		return nil, fmt.Errorf("invalid device id %q", id)
	}
	return config.Load(id, path)
}

func isPath(arg string) bool {
	if strings.ContainsRune(arg, os.PathSeparator) || strings.HasSuffix(arg, ".yml") || strings.HasSuffix(arg, ".yaml") {
		return true
	}
	_, err := os.Stat(arg)
	return err == nil
}
