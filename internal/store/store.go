// Package store persists the small amount of per-host state autotun
// remembers between runs — currently which scheme a remote port speaks.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
)

// FileName is the store's basename inside the user's config directory.
const FileName = "schemes.json"

// Store maps "<host>:<remote port>" to a remembered value.
//
// It is deliberately tiny and forgiving: a corrupt or unreadable file is
// treated as empty rather than as a startup failure, because losing the memory
// of which port serves HTTPS should never stop you connecting.
type Store struct {
	path string

	mu     sync.Mutex
	values map[string]string
	dirty  bool
}

// DefaultPath returns the store's location, honoring XDG on Unix and
// %AppData% on Windows via os.UserConfigDir.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "autotun", FileName), nil
}

// Open loads the store at path, returning an empty one if it does not exist.
func Open(path string) *Store {
	s := &Store{path: path, values: map[string]string{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var parsed map[string]string
	if err := json.Unmarshal(data, &parsed); err != nil {
		return s
	}
	for k, v := range parsed {
		s.values[k] = v
	}
	return s
}

// OpenDefault loads the store from DefaultPath. It never fails: an
// undiscoverable config directory yields an in-memory-only store.
func OpenDefault() *Store {
	path, err := DefaultPath()
	if err != nil {
		return &Store{values: map[string]string{}}
	}
	return Open(path)
}

// key builds the lookup key for a host and remote port.
func key(host string, port int) string {
	return host + ":" + strconv.Itoa(port)
}

// Get returns the remembered value for a host and port.
func (s *Store) Get(host string, port int) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[key(host, port)]
	return v, ok
}

// Set records a value. An empty value forgets the entry, so cycling a port back
// to its default does not leave clutter behind.
func (s *Store) Set(host string, port int, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(host, port)
	if value == "" {
		if _, existed := s.values[k]; !existed {
			return
		}
		delete(s.values, k)
	} else {
		if s.values[k] == value {
			return
		}
		s.values[k] = value
	}
	s.dirty = true
}

// Len reports how many entries are held.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.values)
}

// Save writes the store back to disk if anything changed. It is safe to call
// on a store with no path, where it does nothing.
func (s *Store) Save() error {
	s.mu.Lock()
	if !s.dirty || s.path == "" {
		s.mu.Unlock()
		return nil
	}
	// Marshal a sorted copy so the file has a stable diff.
	keys := make([]string, 0, len(s.values))
	for k := range s.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make(map[string]string, len(keys))
	for _, k := range keys {
		ordered[k] = s.values[k]
	}
	s.dirty = false
	path := s.path
	s.mu.Unlock()

	data, err := json.MarshalIndent(ordered, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Write via a temporary file so an interrupted save cannot truncate the
	// existing store.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".autotun-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
