package app

import (
	"fmt"
	"io"
	"time"

	"github.com/jclement/autotun/internal/sshx"
	"github.com/jclement/autotun/internal/tunnel"
	"github.com/spf13/pflag"
)

// Config is the fully parsed command line.
type Config struct {
	Destination string

	// SSH
	User           string
	Port           int
	IdentityFiles  []string
	ProxyJump      string
	InsecureHost   bool
	AcceptNewHost  bool
	StrictHost     bool
	ConnectTimeout time.Duration

	// Forwarding
	Bind       string
	Existing   bool
	Include    string
	Exclude    string
	MinPort    int
	MaxPort    int
	RemoteBind string
	SamePort   bool
	Interval   time.Duration
	NoDetect   bool

	// Output
	Plain      bool
	JSON       bool
	NoDissolve bool
	NoColor    bool
	Version    bool
}

// Flags describes the command line. Split out so tests can parse argument
// vectors without going near a terminal.
func (c *Config) Flags(name string, errOut io.Writer) *pflag.FlagSet {
	fs := pflag.NewFlagSet(name, pflag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.SortFlags = false

	fs.StringVarP(&c.User, "user", "l", "", "remote user (overrides ssh_config)")
	fs.IntVarP(&c.Port, "port", "p", 0, "SSH port (overrides ssh_config)")
	fs.StringArrayVarP(&c.IdentityFiles, "identity", "i", nil, "private key file (repeatable)")
	fs.StringVarP(&c.ProxyJump, "jump", "J", "", "connect via this jump host")
	fs.DurationVar(&c.ConnectTimeout, "connect-timeout", 20*time.Second, "SSH connect timeout")
	fs.BoolVar(&c.AcceptNewHost, "accept-new-host-key", false, "trust unknown host keys without asking")
	fs.BoolVar(&c.StrictHost, "strict-host-key", false, "refuse hosts missing from known_hosts")
	fs.BoolVar(&c.InsecureHost, "insecure-host-key", false, "skip host key verification entirely")

	fs.StringVarP(&c.Bind, "bind", "b", "127.0.0.1", "local address to bind forwarded ports to")
	fs.BoolVar(&c.Existing, "existing", false, "also forward ports already listening at connect time")
	fs.StringVar(&c.Include, "include", "", "only forward these ports, e.g. 3000,8000-9000")
	fs.StringVar(&c.Exclude, "exclude", "", "never forward these ports")
	fs.IntVar(&c.MinPort, "min-port", 1024, "ignore remote ports below this")
	fs.IntVar(&c.MaxPort, "max-port", 65535, "ignore remote ports above this")
	fs.StringVar(&c.RemoteBind, "remote-bind", string(tunnel.BindAny), `which remote binds to forward: "any" or "loopback"`)
	fs.BoolVar(&c.SamePort, "same-port", false, "never remap; a busy local port is an error")
	fs.DurationVar(&c.Interval, "interval", 2*time.Second, "how often to scan the remote for new ports")
	fs.BoolVar(&c.NoDetect, "no-detect", false, "do not probe new tunnels to detect HTTPS")

	fs.BoolVar(&c.Plain, "plain", false, "line-oriented output instead of the TUI")
	fs.BoolVar(&c.JSON, "json", false, "newline-delimited JSON events (implies --plain)")
	fs.BoolVar(&c.NoDissolve, "no-dissolve", false, "skip the exit animation")
	fs.BoolVar(&c.NoColor, "no-color", false, "disable color output")
	fs.BoolVarP(&c.Version, "version", "V", false, "print version and exit")

	return fs
}

// Validate checks the parsed configuration and returns the derived policy.
func (c *Config) Validate() (tunnel.Policy, error) {
	var p tunnel.Policy

	if c.MinPort < 1 || c.MinPort > 65535 {
		return p, fmt.Errorf("--min-port must be between 1 and 65535")
	}
	if c.MaxPort < 1 || c.MaxPort > 65535 {
		return p, fmt.Errorf("--max-port must be between 1 and 65535")
	}
	if c.MinPort > c.MaxPort {
		return p, fmt.Errorf("--min-port %d is above --max-port %d", c.MinPort, c.MaxPort)
	}
	if c.Interval < 200*time.Millisecond {
		return p, fmt.Errorf("--interval must be at least 200ms")
	}

	include, err := tunnel.ParsePortSet(c.Include)
	if err != nil {
		return p, fmt.Errorf("--include: %w", err)
	}
	exclude, err := tunnel.ParsePortSet(c.Exclude)
	if err != nil {
		return p, fmt.Errorf("--exclude: %w", err)
	}
	rb, err := tunnel.ParseRemoteBind(c.RemoteBind)
	if err != nil {
		return p, err
	}
	if n := boolCount(c.InsecureHost, c.StrictHost, c.AcceptNewHost); n > 1 {
		return p, fmt.Errorf("--insecure-host-key, --strict-host-key and --accept-new-host-key are mutually exclusive")
	}

	return tunnel.Policy{
		MinPort:    c.MinPort,
		MaxPort:    c.MaxPort,
		Include:    include,
		Exclude:    exclude,
		RemoteBind: rb,
		Existing:   c.Existing,
	}, nil
}

// HostKeyPolicy maps the host-key flags onto the sshx policy. An empty result
// means "use whatever ssh_config says".
func (c *Config) HostKeyPolicy() sshx.HostKeyPolicy {
	switch {
	case c.InsecureHost:
		return sshx.HostKeyNone
	case c.StrictHost:
		return sshx.HostKeyStrict
	case c.AcceptNewHost:
		return sshx.HostKeyAcceptNew
	default:
		return ""
	}
}

func boolCount(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

// Usage is the help text shown above the flag list.
const Usage = `autotun — forward every port your remote dev box opens, automatically.

Usage:
  autotun [flags] <destination>

  <destination>  user@host, host, host:port, or an ssh_config alias.

Examples:
  autotun devbox                       forward new ports as they appear
  autotun --existing devbox            include ports already listening
  autotun --include 3000,8000-9000 dev only these ports
  autotun --bind 0.0.0.0 devbox        share the tunnels on your LAN
  autotun --json devbox | jq .         machine-readable event stream

Flags:`
