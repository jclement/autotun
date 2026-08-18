// Package config persists per-host settings — which view a host opens in, and
// per-port protocol, forwarding mode and preferred local port.
//
// The file is human-editable on purpose: it is the record of decisions you made
// in the UI, and being able to read, diff and hand-edit it is worth more than a
// compact encoding.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"gopkg.in/yaml.v3"
)

// FileName is the config's basename inside the user's config directory.
const FileName = "hosts.yml"

// legacySchemes is the file an earlier version wrote; it is imported once.
const legacySchemes = "schemes.json"

// View selects which services a host shows by default.
type View string

const (
	// ViewSinceStart shows only services that appeared after connecting.
	ViewSinceStart View = "since-start"
	// ViewEverything also shows what was already listening.
	ViewEverything View = "everything"
)

// ParseView validates a stored view, defaulting to since-start.
func ParseView(s string) View {
	if View(s) == ViewEverything {
		return ViewEverything
	}
	return ViewSinceStart
}

// Mode is a port's forwarding decision.
type Mode string

const (
	// ModeAuto follows the policy: forward new services, skip pre-existing.
	ModeAuto Mode = "auto"
	// ModeOn always forwards, whatever the policy says.
	ModeOn Mode = "on"
	// ModeOff never forwards.
	ModeOff Mode = "off"
)

// ParseMode validates a stored mode, defaulting to auto.
func ParseMode(s string) Mode {
	switch Mode(s) {
	case ModeOn:
		return ModeOn
	case ModeOff:
		return ModeOff
	default:
		return ModeAuto
	}
}

// Next cycles auto → on → off → auto. The zero value counts as auto, so an
// unset mode behaves the same as an explicitly automatic one.
func (m Mode) Next() Mode {
	switch ParseMode(string(m)) {
	case ModeOn:
		return ModeOff
	case ModeOff:
		return ModeAuto
	default:
		return ModeOn
	}
}

// Port holds the remembered settings for one remote port.
type Port struct {
	// Label is a human-friendly name for the service, such as "frontend".
	Label string `yaml:"label,omitempty"`
	// Scheme is "http" or "https"; empty means undetermined.
	Scheme string `yaml:"scheme,omitempty"`
	// Mode is auto, on or off; empty means auto.
	Mode string `yaml:"mode,omitempty"`
	// Local is a preferred local port; zero means mirror the remote port.
	Local int `yaml:"local,omitempty"`
}

// IsZero reports whether the entry holds nothing worth persisting.
func (p Port) IsZero() bool {
	return p.Label == "" && p.Scheme == "" &&
		(p.Mode == "" || Mode(p.Mode) == ModeAuto) && p.Local == 0
}

// Host holds the remembered settings for one destination.
type Host struct {
	// View is the older name for ShowPreexisting; it is still read so an
	// existing config keeps working.
	View string `yaml:"view,omitempty"`

	ShowPreexisting bool   `yaml:"show_preexisting,omitempty"`
	Sort            string `yaml:"sort,omitempty"`
	Reverse         bool   `yaml:"reverse,omitempty"`
	// InactiveLast defaults to true, so it is a pointer: absent means "not
	// configured", which is different from "explicitly off".
	InactiveLast *bool `yaml:"inactive_last,omitempty"`

	Ports map[int]Port `yaml:"ports,omitempty"`
}

// IsZero reports whether the host entry holds nothing worth persisting.
func (h Host) IsZero() bool {
	return h.View == "" && !h.ShowPreexisting && h.Sort == "" && !h.Reverse &&
		h.InactiveLast == nil && len(h.Ports) == 0
}

// ViewPrefs is how a host's table is presented. These are display choices
// only: none of them forwards or stops forwarding anything.
type ViewPrefs struct {
	// ShowPreexisting lists services that were already running when autotun
	// connected. They are still not forwarded — that needs --existing, or
	// setting the port to "on".
	ShowPreexisting bool
	// InactiveLast sinks rows with no tunnel below the ones that have them.
	InactiveLast bool
	// Sort names the ordering, e.g. "port" or "recent".
	Sort    string
	Reverse bool
}

// DefaultViewPrefs is how a host is presented before anyone changes anything.
func DefaultViewPrefs() ViewPrefs {
	return ViewPrefs{InactiveLast: true, Sort: "port"}
}

// ViewPrefs returns a host's presentation settings.
func (s *Store) ViewPrefs(host string) ViewPrefs {
	s.mu.Lock()
	defer s.mu.Unlock()

	h := s.hosts[host]
	p := DefaultViewPrefs()
	// The older "view: everything" meant the same thing this now calls
	// showing pre-existing services.
	p.ShowPreexisting = h.ShowPreexisting || ParseView(h.View) == ViewEverything
	if h.Sort != "" {
		p.Sort = h.Sort
	}
	p.Reverse = h.Reverse
	if h.InactiveLast != nil {
		p.InactiveLast = *h.InactiveLast
	}
	return p
}

// SetViewPrefs records a host's presentation settings.
func (s *Store) SetViewPrefs(host string, p ViewPrefs) {
	s.mu.Lock()
	defer s.mu.Unlock()

	h := s.hosts[host]
	next := h
	next.View = "" // superseded by ShowPreexisting
	next.ShowPreexisting = p.ShowPreexisting
	next.Sort = ""
	if p.Sort != "" && p.Sort != DefaultViewPrefs().Sort {
		next.Sort = p.Sort
	}
	next.Reverse = p.Reverse
	next.InactiveLast = nil
	if !p.InactiveLast {
		off := false
		next.InactiveLast = &off
	}

	if next.View == h.View && next.ShowPreexisting == h.ShowPreexisting &&
		next.Sort == h.Sort && next.Reverse == h.Reverse &&
		samePtr(next.InactiveLast, h.InactiveLast) {
		return
	}
	if next.IsZero() {
		delete(s.hosts, host)
	} else {
		s.hosts[host] = next
	}
	s.dirty = true
}

func samePtr(a, b *bool) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

// document is the on-disk shape.
type document struct {
	Hosts map[string]Host `yaml:"hosts"`
}

// Store is a loaded config file.
//
// It is deliberately forgiving: an unreadable or malformed file is treated as
// empty rather than as a startup failure, because losing the memory of which
// port serves HTTPS should never stop you connecting.
type Store struct {
	path string

	mu    sync.Mutex
	hosts map[string]Host
	dirty bool
}

// DefaultPath returns the config's location, honoring XDG on Unix and
// %AppData% on Windows via os.UserConfigDir.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "autotun", FileName), nil
}

// Open loads the config at path, returning an empty store if it is missing or
// unreadable.
func Open(path string) *Store {
	s := &Store{path: path, hosts: map[string]Host{}}

	data, err := os.ReadFile(path)
	if err != nil {
		s.importLegacy()
		return s
	}
	var doc document
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return s
	}
	for name, h := range doc.Hosts {
		if h.Ports == nil {
			h.Ports = map[int]Port{}
		}
		s.hosts[name] = h
	}
	return s
}

// OpenDefault loads the config from DefaultPath. It never fails: an
// undiscoverable config directory yields an in-memory-only store.
func OpenDefault() *Store {
	path, err := DefaultPath()
	if err != nil {
		return &Store{hosts: map[string]Host{}}
	}
	return Open(path)
}

// importLegacy carries forward the flat scheme map an earlier version wrote,
// so upgrading does not silently forget which ports were marked HTTPS.
func (s *Store) importLegacy() {
	if s.path == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(s.path), legacySchemes))
	if err != nil {
		return
	}
	var flat map[string]string
	if err := json.Unmarshal(data, &flat); err != nil {
		return
	}
	for key, scheme := range flat {
		host, port, ok := splitLegacyKey(key)
		if !ok || scheme == "" {
			continue
		}
		h := s.hosts[host]
		if h.Ports == nil {
			h.Ports = map[int]Port{}
		}
		entry := h.Ports[port]
		entry.Scheme = scheme
		h.Ports[port] = entry
		s.hosts[host] = h
		s.dirty = true
	}
}

// splitLegacyKey parses the old "host:port" key, which may itself contain
// colons for an IPv6 literal.
func splitLegacyKey(key string) (string, int, bool) {
	i := lastIndexByte(key, ':')
	if i <= 0 {
		return "", 0, false
	}
	port := 0
	if _, err := fmt.Sscanf(key[i+1:], "%d", &port); err != nil || port <= 0 {
		return "", 0, false
	}
	return key[:i], port, true
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// View returns a host's default view.
func (s *Store) View(host string) View {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ParseView(s.hosts[host].View)
}

// SetView records a host's default view.
func (s *Store) SetView(host string, v View) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.hosts[host]
	next := ""
	if v == ViewEverything {
		next = string(ViewEverything)
	}
	if h.View == next {
		return
	}
	h.View = next
	s.hosts[host] = h
	s.dirty = true
}

// Port returns the remembered settings for a host's remote port.
func (s *Store) Port(host string, port int) Port {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hosts[host].Ports[port]
}

// SetPort records settings for a host's remote port. An entry holding nothing
// is removed rather than persisted as clutter.
func (s *Store) SetPort(host string, port int, p Port) {
	s.mu.Lock()
	defer s.mu.Unlock()

	h := s.hosts[host]
	if h.Ports == nil {
		h.Ports = map[int]Port{}
	}
	if p.IsZero() {
		if _, existed := h.Ports[port]; !existed {
			return
		}
		delete(h.Ports, port)
	} else {
		if h.Ports[port] == p {
			return
		}
		h.Ports[port] = p
	}
	if h.IsZero() {
		delete(s.hosts, host)
	} else {
		s.hosts[host] = h
	}
	s.dirty = true
}

// Hosts returns the configured host names, sorted.
func (s *Store) Hosts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.hosts))
	for name := range s.hosts {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Path reports where the store is saved, or "" if it is memory-only.
func (s *Store) Path() string { return s.path }

// Save writes the config back if anything changed.
func (s *Store) Save() error {
	s.mu.Lock()
	if !s.dirty || s.path == "" {
		s.mu.Unlock()
		return nil
	}
	doc := document{Hosts: map[string]Host{}}
	for name, h := range s.hosts {
		doc.Hosts[name] = h
	}
	s.dirty = false
	path := s.path
	s.mu.Unlock()

	var buf []byte
	body, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	buf = append(buf, "# autotun per-host settings. Safe to edit by hand.\n"...)
	buf = append(buf, body...)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Write via a temporary file so an interrupted save cannot truncate the
	// existing config.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".autotun-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(buf); err != nil {
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
