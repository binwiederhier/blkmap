package source

import (
	"fmt"
	"sync"
)

// Swappable is a Source whose target can be replaced while a device is live. Reads in
// flight on the old target finish before Swap returns.
type Swappable struct {
	src Source
	mu  sync.RWMutex // Protects src; readers hold it for the duration of a read
}

func NewSwappable(src Source) *Swappable {
	return &Swappable{src: src}
}

// Swap installs next and returns the previous source, which the caller owns from then on.
// The sizes must match, since the device size is fixed.
func (s *Swappable) Swap(next Source) (Source, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if next.Size() != s.src.Size() {
		return nil, fmt.Errorf("cannot swap in a source of size %d for one of size %d", next.Size(), s.src.Size())
	}
	old := s.src
	s.src = next
	return old, nil
}

func (s *Swappable) ReadAt(p []byte, off int64) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.src.ReadAt(p, off)
}

// ReadAtDirect forwards a cache-bypassing read to the current target.
func (s *Swappable) ReadAtDirect(p []byte, off int64) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return ReadDirect(s.src, p, off)
}

func (s *Swappable) Holes(off, length int64) ([]Range, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Holes(s.src, off, length)
}

func (s *Swappable) Size() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.src.Size()
}

func (s *Swappable) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.src.Close()
}
