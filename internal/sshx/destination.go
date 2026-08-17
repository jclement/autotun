// Package sshx resolves SSH destinations against ssh_config and establishes
// the connection autotun multiplexes its prober and forwards over.
package sshx

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kevinburke/ssh_config"
)

// Destination is a fully resolved SSH target.
type Destination struct {
	// Alias is the name the user typed, used for display and for looking up
	// ssh_config stanzas.
	Alias         string
	User          string
	Host          string
	Port          int
	IdentityFiles []string
	ProxyJump     string
	StrictHostKey string // "yes", "no", "accept-new", or "" for the default
	// IdentitiesOnly suppresses the SSH agent, offering only the configured
	// keys. Passing -i explicitly implies it, matching ssh(1): otherwise a
	// well-stocked agent burns through the server's MaxAuthTries before the
	// key you actually named is ever tried.
	IdentitiesOnly bool
}

// Addr renders host:port.
func (d Destination) Addr() string {
	return joinHostPort(d.Host, d.Port)
}

// String renders the destination the way a user would type it.
func (d Destination) String() string {
	s := d.Host
	if d.User != "" {
		s = d.User + "@" + s
	}
	if d.Port != 22 {
		s += ":" + strconv.Itoa(d.Port)
	}
	return s
}

// Label is the short name shown in the UI.
func (d Destination) Label() string {
	if d.Alias != "" && d.Alias != d.Host {
		return d.Alias
	}
	return d.Host
}

func joinHostPort(host string, port int) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return host + ":" + strconv.Itoa(port)
}

// Overrides are command-line values that win over ssh_config.
type Overrides struct {
	User          string
	Port          int
	IdentityFiles []string
	ProxyJump     string
}

// configGetter reads ssh_config keys for a host. Nil means "no config".
type configGetter interface {
	Get(alias, key string) (string, error)
}

// ParseTarget splits a destination as typed into its user, host and port parts.
// Accepted forms: "host", "user@host", "host:port", "user@host:port",
// "ssh://user@host:port", and bracketed IPv6 literals.
func ParseTarget(raw string) (user, host string, port int, err error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", "", 0, fmt.Errorf("empty destination")
	}
	s = strings.TrimPrefix(s, "ssh://")
	if u, rest, ok := strings.Cut(s, "@"); ok {
		if u == "" {
			return "", "", 0, fmt.Errorf("destination %q has an empty user", raw)
		}
		user, s = u, rest
	}
	if s == "" {
		return "", "", 0, fmt.Errorf("destination %q has an empty host", raw)
	}
	// Bracketed IPv6: [::1] or [::1]:2222
	if strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end < 0 {
			return "", "", 0, fmt.Errorf("destination %q has an unterminated [", raw)
		}
		host = s[1:end]
		rest := s[end+1:]
		if rest != "" {
			if !strings.HasPrefix(rest, ":") {
				return "", "", 0, fmt.Errorf("unexpected %q after address in %q", rest, raw)
			}
			port, err = parsePortStr(rest[1:], raw)
		}
		return user, host, port, err
	}
	// A bare IPv6 literal has several colons; only treat a single trailing
	// colon as a port separator.
	if strings.Count(s, ":") == 1 {
		h, p, _ := strings.Cut(s, ":")
		port, err = parsePortStr(p, raw)
		return user, h, port, err
	}
	return user, s, 0, nil
}

func parsePortStr(s, raw string) (int, error) {
	p, err := strconv.Atoi(s)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("destination %q has an invalid port %q", raw, s)
	}
	return p, nil
}

// Resolve turns a typed destination into a Destination, layering ssh_config
// under the command-line overrides.
func Resolve(raw string, cfg configGetter, ov Overrides) (*Destination, error) {
	typedUser, alias, typedPort, err := ParseTarget(raw)
	if err != nil {
		return nil, err
	}

	d := &Destination{Alias: alias, Host: alias, User: typedUser, Port: typedPort}

	if cfg != nil {
		if v, err := cfg.Get(alias, "HostName"); err == nil && v != "" {
			// OpenSSH expands %h in HostName to the alias.
			d.Host = strings.ReplaceAll(v, "%h", alias)
		}
		if d.User == "" {
			if v, err := cfg.Get(alias, "User"); err == nil && v != "" {
				d.User = v
			}
		}
		if d.Port == 0 {
			if v, err := cfg.Get(alias, "Port"); err == nil && v != "" {
				if p, err := strconv.Atoi(v); err == nil {
					d.Port = p
				}
			}
		}
		if v, err := cfg.Get(alias, "IdentityFile"); err == nil && v != "" {
			d.IdentityFiles = append(d.IdentityFiles, ExpandPath(v))
		}
		if v, err := cfg.Get(alias, "ProxyJump"); err == nil && v != "" && !strings.EqualFold(v, "none") {
			d.ProxyJump = v
		}
		if v, err := cfg.Get(alias, "StrictHostKeyChecking"); err == nil {
			d.StrictHostKey = strings.ToLower(v)
		}
		if v, err := cfg.Get(alias, "IdentitiesOnly"); err == nil && strings.EqualFold(v, "yes") {
			d.IdentitiesOnly = true
		}
	}

	if ov.User != "" {
		d.User = ov.User
	}
	if ov.Port != 0 {
		d.Port = ov.Port
	}
	if ov.ProxyJump != "" {
		d.ProxyJump = ov.ProxyJump
	}
	if len(ov.IdentityFiles) > 0 {
		// An explicit -i replaces rather than augments, matching ssh(1), and
		// implies IdentitiesOnly.
		d.IdentityFiles = nil
		d.IdentitiesOnly = true
		for _, f := range ov.IdentityFiles {
			d.IdentityFiles = append(d.IdentityFiles, ExpandPath(f))
		}
	}

	if d.Port == 0 {
		d.Port = 22
	}
	if d.User == "" {
		d.User = currentUsername()
	}
	if d.User == "" {
		return nil, fmt.Errorf("cannot determine a username for %q; pass --user", raw)
	}
	return d, nil
}

// ExpandPath resolves ~ and environment variables in a path.
func ExpandPath(p string) string {
	p = strings.Trim(p, `"`)
	p = os.ExpandEnv(p)
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p[1:], string(filepath.Separator)))
		}
	}
	return p
}

func currentUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		name := u.Username
		// Windows reports DOMAIN\user.
		if i := strings.LastIndexAny(name, `\/`); i >= 0 {
			name = name[i+1:]
		}
		return name
	}
	for _, k := range []string{"USER", "LOGNAME", "USERNAME"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// LoadConfig reads the user and system ssh_config files. A missing file is not
// an error; a malformed one is.
func LoadConfig() (configGetter, error) {
	var configs []*ssh_config.Config
	paths := []string{}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".ssh", "config"))
	}
	paths = append(paths, "/etc/ssh/ssh_config")

	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		cfg, err := ssh_config.Decode(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", p, err)
		}
		configs = append(configs, cfg)
	}
	if len(configs) == 0 {
		return nil, nil
	}
	return chainConfig(configs), nil
}

// chainConfig queries several ssh_config files in order, first match wins.
type chainConfig []*ssh_config.Config

func (c chainConfig) Get(alias, key string) (string, error) {
	for _, cfg := range c {
		v, err := cfg.Get(alias, key)
		if err != nil {
			return "", err
		}
		if v != "" {
			return v, nil
		}
	}
	return "", nil
}
