package source

import (
	"fmt"
	"sync"
)

// Constructor builds a custom source for a "type: custom" segment. size is the configured
// size (0 if none) and params the free-form strings from the config.
type Constructor func(size int64, params map[string]string) (Source, error)

var (
	customs   = map[string]Constructor{}
	customsMu sync.Mutex // Protects customs
)

// Register makes a constructor available to configs under name. Call it before loading a
// config that refers to the name.
func Register(name string, ctor Constructor) {
	customsMu.Lock()
	defer customsMu.Unlock()
	customs[name] = ctor
}

// NewCustom instantiates a registered custom source.
func NewCustom(name string, size int64, params map[string]string) (Source, error) {
	customsMu.Lock()
	ctor, ok := customs[name]
	customsMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no custom source registered as %q", name)
	}
	return ctor(size, params)
}
