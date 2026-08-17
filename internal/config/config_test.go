package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)

	s := Open(path)
	s.SetView("devbox", ViewEverything)
	s.SetPort("devbox", 3000, Port{Scheme: "https", Mode: string(ModeOn), Local: 13000})
	s.SetPort("devbox", 5432, Port{Mode: string(ModeOff)})
	s.SetPort("other", 8080, Port{Scheme: "http"})
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got := Open(path)
	if got.View("devbox") != ViewEverything {
		t.Errorf("view = %q, want everything", got.View("devbox"))
	}
	if got.View("other") != ViewSinceStart {
		t.Errorf("an unset view = %q, want since-start", got.View("other"))
	}

	p := got.Port("devbox", 3000)
	if p.Scheme != "https" || ParseMode(p.Mode) != ModeOn || p.Local != 13000 {
		t.Errorf("port 3000 = %+v", p)
	}
	if p := got.Port("devbox", 5432); ParseMode(p.Mode) != ModeOff {
		t.Errorf("port 5432 mode = %q, want off", p.Mode)
	}
	// Settings are scoped to a host.
	if p := got.Port("other", 3000); !p.IsZero() {
		t.Errorf("settings leaked between hosts: %+v", p)
	}
}

func TestSetPortRemovesEmptyEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)

	s := Open(path)
	s.SetPort("devbox", 3000, Port{Scheme: "https"})
	s.SetPort("devbox", 3000, Port{})
	if p := s.Port("devbox", 3000); !p.IsZero() {
		t.Errorf("an emptied entry should be gone, got %+v", p)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if hosts := Open(path).Hosts(); len(hosts) != 0 {
		t.Errorf("hosts = %v, want the empty host dropped", hosts)
	}
}

// An explicitly automatic mode carries no information, so it is not persisted.
func TestAutoModeIsNotPersisted(t *testing.T) {
	if !(Port{Mode: string(ModeAuto)}).IsZero() {
		t.Error("an auto-only entry should count as empty")
	}
	if (Port{Mode: string(ModeOff)}).IsZero() {
		t.Error("an off entry is worth keeping")
	}
	if (Port{Local: 1234}).IsZero() {
		t.Error("a pinned local port is worth keeping")
	}
}

func TestModeCycle(t *testing.T) {
	tests := []struct {
		in   Mode
		want Mode
	}{
		{ModeAuto, ModeOn},
		{ModeOn, ModeOff},
		{ModeOff, ModeAuto},
		// The zero value must behave as auto, or the first press skips a step.
		{Mode(""), ModeOn},
		{Mode("nonsense"), ModeOn},
	}
	for _, tt := range tests {
		if got := tt.in.Next(); got != tt.want {
			t.Errorf("Mode(%q).Next() = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseModeAndView(t *testing.T) {
	for in, want := range map[string]Mode{
		"on": ModeOn, "off": ModeOff, "auto": ModeAuto, "": ModeAuto, "weird": ModeAuto,
	} {
		if got := ParseMode(in); got != want {
			t.Errorf("ParseMode(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]View{
		"everything": ViewEverything, "since-start": ViewSinceStart, "": ViewSinceStart, "x": ViewSinceStart,
	} {
		if got := ParseView(in); got != want {
			t.Errorf("ParseView(%q) = %q, want %q", in, got, want)
		}
	}
}

// A broken config must never stop autotun from starting.
func TestToleratesACorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte("hosts: [this is not: valid: yaml"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := Open(path)
	if len(s.Hosts()) != 0 {
		t.Errorf("a corrupt config should read as empty, got %v", s.Hosts())
	}
	s.SetPort("devbox", 3000, Port{Scheme: "https"})
	if err := s.Save(); err != nil {
		t.Fatalf("Save over a corrupt file: %v", err)
	}
	if p := Open(path).Port("devbox", 3000); p.Scheme != "https" {
		t.Errorf("after recovery = %+v", p)
	}
}

func TestMissingFileIsEmpty(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "nope", FileName))
	if len(s.Hosts()) != 0 {
		t.Errorf("a missing config has %v", s.Hosts())
	}
	s.SetPort("devbox", 3000, Port{Scheme: "http"})
	if err := s.Save(); err != nil {
		t.Errorf("Save should create missing directories: %v", err)
	}
}

func TestSaveIsANoopWhenUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s := Open(path)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("saving an untouched config should not create a file")
	}

	// Re-setting the same value must not mark it dirty.
	s.SetPort("devbox", 3000, Port{Scheme: "http"})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	s.SetPort("devbox", 3000, Port{Scheme: "http"})
	s.SetView("devbox", ViewSinceStart)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("a redundant write caused a rewrite")
	}
}

// The file is meant to be read and edited by hand.
func TestSavedFileIsReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s := Open(path)
	s.SetPort("devbox", 3000, Port{Scheme: "https", Local: 13000})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, want := range []string{"# autotun", "hosts:", "devbox:", "3000:", "scheme: https", "local: 13000"} {
		if !strings.Contains(body, want) {
			t.Errorf("the saved config is missing %q:\n%s", want, body)
		}
	}
}

func TestFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not carry Unix permission bits")
	}
	path := filepath.Join(t.TempDir(), FileName)
	s := Open(path)
	s.SetPort("devbox", 3000, Port{Scheme: "http"})
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

// Upgrading must not silently forget the protocols an earlier version stored.
func TestImportsTheLegacySchemeFile(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"devbox:3000":"https","devbox:8080":"http","[::1]:9000":"https","bad":"http"}`
	if err := os.WriteFile(filepath.Join(dir, legacySchemes), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	s := Open(filepath.Join(dir, FileName))
	if got := s.Port("devbox", 3000).Scheme; got != "https" {
		t.Errorf("devbox:3000 scheme = %q, want https", got)
	}
	if got := s.Port("devbox", 8080).Scheme; got != "http" {
		t.Errorf("devbox:8080 scheme = %q, want http", got)
	}
	// A host name containing colons still parses.
	if got := s.Port("[::1]", 9000).Scheme; got != "https" {
		t.Errorf("[::1]:9000 scheme = %q, want https", got)
	}

	// And the import is persisted, so the legacy file stops mattering.
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if got := Open(filepath.Join(dir, FileName)).Port("devbox", 3000).Scheme; got != "https" {
		t.Errorf("the imported scheme was not saved, got %q", got)
	}
}

// A real config wins over the legacy file rather than being merged with it.
func TestLegacyImportSkippedWhenConfigExists(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, legacySchemes), []byte(`{"devbox:3000":"https"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("hosts: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if p := Open(filepath.Join(dir, FileName)).Port("devbox", 3000); !p.IsZero() {
		t.Errorf("the legacy file was imported over a real config: %+v", p)
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

func TestOpenDefaultNeverFails(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if s := OpenDefault(); s == nil {
		t.Fatal("OpenDefault returned nil")
	}
}

func TestStoreIsConcurrencySafe(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), FileName))
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 100; j++ {
				port := n*100 + j
				s.SetPort("devbox", port, Port{Scheme: "http"})
				s.Port("devbox", port)
				s.SetView("devbox", ViewEverything)
				s.View("devbox")
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if got := len(s.Hosts()); got != 1 {
		t.Errorf("hosts = %d, want 1", got)
	}
}
