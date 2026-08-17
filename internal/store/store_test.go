package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schemes.json")

	s := Open(path)
	if s.Len() != 0 {
		t.Fatalf("a fresh store has %d entries, want 0", s.Len())
	}
	s.Set("devbox", 3000, "https")
	s.Set("devbox", 8080, "http")
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reopened := Open(path)
	if got, ok := reopened.Get("devbox", 3000); !ok || got != "https" {
		t.Errorf("Get(devbox, 3000) = %q, %v; want https", got, ok)
	}
	if got, ok := reopened.Get("devbox", 8080); !ok || got != "http" {
		t.Errorf("Get(devbox, 8080) = %q, %v; want http", got, ok)
	}
	if _, ok := reopened.Get("devbox", 9999); ok {
		t.Error("Get returned a value for a port never set")
	}
	if _, ok := reopened.Get("otherhost", 3000); ok {
		t.Error("entries must be scoped by host")
	}
}

func TestStoreSetEmptyForgets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schemes.json")

	s := Open(path)
	s.Set("devbox", 3000, "https")
	s.Set("devbox", 3000, "")
	if _, ok := s.Get("devbox", 3000); ok {
		t.Error("an empty value should remove the entry")
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if Open(path).Len() != 0 {
		t.Error("the removal was not persisted")
	}
}

func TestStoreSaveIsANoopWhenUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schemes.json")

	s := Open(path)
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("saving an untouched store should not create a file")
	}

	// Setting a value to what it already is must not mark the store dirty.
	s.Set("devbox", 3000, "http")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Set("devbox", 3000, "http")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	again, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(again.ModTime()) {
		t.Error("a redundant Set caused a rewrite")
	}
}

// A broken store must never stop autotun from starting.
func TestStoreToleratesACorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schemes.json")
	if err := os.WriteFile(path, []byte("{not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := Open(path)
	if s.Len() != 0 {
		t.Errorf("a corrupt store should read as empty, got %d entries", s.Len())
	}

	// And it must still be usable and overwrite the damaged file.
	s.Set("devbox", 3000, "https")
	if err := s.Save(); err != nil {
		t.Fatalf("Save over a corrupt file: %v", err)
	}
	if got, ok := Open(path).Get("devbox", 3000); !ok || got != "https" {
		t.Errorf("after recovery Get = %q, %v", got, ok)
	}
}

func TestStoreMissingFileIsEmpty(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "does", "not", "exist.json"))
	if s.Len() != 0 {
		t.Errorf("a missing store has %d entries, want 0", s.Len())
	}
	// Saving creates the directory tree.
	s.Set("devbox", 3000, "http")
	if err := s.Save(); err != nil {
		t.Errorf("Save should create missing directories: %v", err)
	}
}

func TestStoreWritesStableJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schemes.json")

	s := Open(path)
	s.Set("zeta", 9000, "http")
	s.Set("alpha", 3000, "https")
	s.Set("alpha", 1000, "http")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Sorted keys keep the file diffable if a user versions their config.
	body := string(data)
	if strings.Index(body, "alpha:1000") > strings.Index(body, "zeta:9000") {
		t.Errorf("keys are not sorted:\n%s", body)
	}
	if !strings.HasSuffix(body, "\n") {
		t.Error("the file should end with a newline")
	}
}

func TestStoreFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schemes.json")
	s := Open(path)
	s.Set("devbox", 3000, "http")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

func TestStoreWithNoPathStaysInMemory(t *testing.T) {
	s := &Store{values: map[string]string{}}
	s.Set("devbox", 3000, "http")
	if err := s.Save(); err != nil {
		t.Errorf("Save on a pathless store = %v, want nil", err)
	}
	if got, ok := s.Get("devbox", 3000); !ok || got != "http" {
		t.Error("an in-memory store should still answer lookups")
	}
}

func TestOpenDefaultNeverFails(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if s := OpenDefault(); s == nil {
		t.Fatal("OpenDefault returned nil")
	}
}

func TestDefaultPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)

	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	if filepath.Base(got) != FileName {
		t.Errorf("DefaultPath = %q, want it to end in %s", got, FileName)
	}
	if !strings.Contains(got, "autotun") {
		t.Errorf("DefaultPath = %q, want it under an autotun directory", got)
	}
}

func TestStoreIsConcurrencySafe(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "schemes.json"))
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 100; j++ {
				s.Set("devbox", n*100+j, "http")
				s.Get("devbox", n*100+j)
				s.Len()
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if s.Len() != 800 {
		t.Errorf("Len() = %d, want 800", s.Len())
	}
}
