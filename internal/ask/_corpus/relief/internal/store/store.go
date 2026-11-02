// Package store keeps one device's replica in a directory: every op it has
// ever seen, appended to a JSON file written atomically.
//
// It also reads and writes bundles: a file of ops plus a checksum, for
// moving changes between devices with no network at all (USB stick,
// Bluetooth file share, a phone carried between camps).
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Aravindh97/relief/internal/crdt"
)

type file struct {
	Device string    `json:"device"`
	Ops    []crdt.Op `json:"ops"`
}

type Store struct {
	Dir string
}

func (s Store) path() string { return filepath.Join(s.Dir, "replica.json") }
func (s Store) lock() string { return filepath.Join(s.Dir, ".lock") }

var ErrLocked = errors.New("replica is locked by another relief process (delete .lock if none is running)")

// With loads the replica, runs fn, and saves it, holding an exclusive lock
// file so two processes on the device cannot overwrite each other's ops.
func (s Store) With(fn func(*crdt.Doc) error) error {
	var lf *os.File
	var err error
	for i := 0; i < 50; i++ {
		lf, err = os.OpenFile(s.lock(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return ErrLocked
	}
	lf.Close()
	defer os.Remove(s.lock())
	d, err := s.load()
	if err != nil {
		return err
	}
	if err := fn(d); err != nil {
		return err
	}
	return s.save(d)
}

func Init(dir, device string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	s := Store{dir}
	if _, err := os.Stat(s.path()); err == nil {
		return fmt.Errorf("%s already has a replica", dir)
	}
	return s.save(crdt.New(device, nil))
}

func (s Store) load() (*crdt.Doc, error) {
	b, err := os.ReadFile(s.path())
	if err != nil {
		return nil, fmt.Errorf("no replica in %s (run `relief init`): %w", s.Dir, err)
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("replica file is corrupt: %w", err)
	}
	d := crdt.New(f.Device, nil)
	if _, err := d.Receive(f.Ops); err != nil {
		return nil, err
	}
	return d, nil
}

func (s Store) save(d *crdt.Doc) error {
	b, err := json.Marshal(file{Device: d.Device, Ops: d.AllOps()})
	if err != nil {
		return err
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path()) // atomic: a crash leaves the old or the new file, never half
}

type Bundle struct {
	From   string    `json:"from"`
	Ops    []crdt.Op `json:"ops"`
	SHA256 string    `json:"sha256"` // detects a truncated or damaged file, not tampering
}

func sum(ops []crdt.Op) string {
	b, _ := json.Marshal(ops)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func WriteBundle(path, from string, ops []crdt.Op) error {
	b, err := json.Marshal(Bundle{From: from, Ops: ops, SHA256: sum(ops)})
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func ReadBundle(path string) (*Bundle, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var bu Bundle
	if err := json.Unmarshal(b, &bu); err != nil {
		return nil, fmt.Errorf("bundle is not valid JSON (damaged?): %w", err)
	}
	if sum(bu.Ops) != bu.SHA256 {
		return nil, errors.New("bundle checksum mismatch: the file was damaged in transfer")
	}
	return &bu, nil
}
