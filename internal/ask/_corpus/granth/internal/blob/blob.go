// Package blob is a content-addressed file store. Activities pass blob hashes
// instead of document bodies, which keeps Temporal payloads small (history
// events have size limits) and makes every write idempotent: the same bytes
// always land at the same path.
package blob

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Aravindh97/granth/internal/doc"
)

var ErrNotFound = errors.New("blob not found")

type Store struct{ Dir string }

func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &Store{Dir: dir}, nil
}

func (s *Store) path(h string) (string, error) {
	if len(h) < 8 {
		return "", fmt.Errorf("invalid blob hash %q", h)
	}
	for _, r := range h {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return "", fmt.Errorf("invalid blob hash %q", h)
		}
	}
	return filepath.Join(s.Dir, h[:2], h), nil
}

// Put stores b and returns its sha256. Writes go to a temp file and are
// renamed into place, so a crash never leaves a half-written blob.
func (s *Store) Put(b []byte) (string, error) {
	h := doc.Hash(b)
	p, _ := s.path(h)
	if _, err := os.Stat(p); err == nil {
		return h, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return "", err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	if err := os.Rename(f.Name(), p); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return h, nil
}

// PutAt stores b under an explicit key (a hash computed by the caller, e.g.
// of the inputs of an embedding batch). Used for caches.
func (s *Store) PutAt(key string, b []byte) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (s *Store) Get(h string) ([]byte, error) {
	p, err := s.path(h)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return b, err
}
