package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jclement/autotun/internal/config"
	"github.com/jclement/autotun/internal/sshx"
	"github.com/jclement/autotun/internal/tunnel"
)

// parse runs the flag set over an argument vector.
func parse(t *testing.T, args ...string) (*Config, []string, error) {
	t.Helper()
	var cfg Config
	fs := cfg.Flags("autotun", io.Discard)
	err := fs.Parse(args)
	return &cfg, fs.Args(), err
}

func TestFlagDefaults(t *testing.T) {
	cfg, args, err := parse(t, "devbox")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(args) != 1 || args[0] != "devbox" {
		t.Errorf("positional args = %v, want [devbox]", args)
	}
	if cfg.Bind != "127.0.0.1" {
		t.Errorf("--bind default = %q, want loopback so tunnels are private", cfg.Bind)
	}
	if cfg.MinPort != 1024 || cfg.MaxPort != 65535 {
		t.Errorf("port window = %d-%d", cfg.MinPort, cfg.MaxPort)
	}
	if cfg.Interval != 2*time.Second {
		t.Errorf("--interval default = %v", cfg.Interval)
	}
	if cfg.Existing {
		t.Error("--existing should be off by default")
	}
}

func TestFlagParsing(t *testing.T) {
	cfg, args, err := parse(t,
		"-l", "root", "-p", "2222", "-i", "/k/a", "-i", "/k/b",
		"-J", "bastion", "-b", "0.0.0.0",
		"--existing", "--include", "3000,8000-9000", "--exclude", "8080",
		"--min-port", "2000", "--max-port", "60000",
		"--remote-bind", "loopback", "--same-port",
		"--interval", "500ms", "--json", "--no-dissolve", "--wait",
		"devbox",
	)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(args) != 1 || args[0] != "devbox" {
		t.Errorf("positional args = %v", args)
	}
	if cfg.User != "root" || cfg.Port != 2222 || cfg.ProxyJump != "bastion" {
		t.Errorf("ssh flags = %+v", cfg)
	}
	if len(cfg.IdentityFiles) != 2 {
		t.Errorf("-i should be repeatable, got %v", cfg.IdentityFiles)
	}
	if cfg.Bind != "0.0.0.0" || !cfg.Existing || !cfg.SamePort || !cfg.JSON {
		t.Errorf("forwarding flags = %+v", cfg)
	}
	if !cfg.Wait {
		t.Error("--wait was not parsed")
	}
	if cfg.Interval != 500*time.Millisecond {
		t.Errorf("--interval = %v", cfg.Interval)
	}
}

func TestConnectInitialWaitsUntilContextEnds(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var out strings.Builder
	_, err = connectInitial(ctx, &sshx.Destination{Host: "127.0.0.1", Port: port, User: "tester"}, sshx.ConnectOptions{
		Timeout: 50 * time.Millisecond, HostKeyPolicy: sshx.HostKeyNone,
	}, true, &out)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline", err)
	}
	if !strings.Contains(out.String(), "retrying in 1s") {
		t.Errorf("--wait did not enter the retry loop:\n%s", out.String())
	}
}

func TestValidateBuildsThePolicy(t *testing.T) {
	cfg, _, err := parse(t, "--include", "3000,8000-9000", "--exclude", "8080",
		"--min-port", "2000", "--max-port", "60000", "--remote-bind", "loopback",
		"--existing", "devbox")
	if err != nil {
		t.Fatal(err)
	}

	p, err := cfg.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if p.MinPort != 2000 || p.MaxPort != 60000 {
		t.Errorf("port window = %d-%d", p.MinPort, p.MaxPort)
	}
	if !p.Include.Contains(8500) || p.Include.Contains(7000) {
		t.Errorf("--include did not become the policy set: %s", p.Include)
	}
	if !p.Exclude.Contains(8080) {
		t.Errorf("--exclude did not become the policy set: %s", p.Exclude)
	}
	if p.RemoteBind != tunnel.BindLoopback {
		t.Errorf("--remote-bind = %q", p.RemoteBind)
	}
	if !p.Existing {
		t.Error("--existing did not reach the policy")
	}
}

func TestValidateRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"min above max", []string{"--min-port", "9000", "--max-port", "80"}, "above"},
		{"min out of range", []string{"--min-port", "0"}, "min-port"},
		{"max out of range", []string{"--max-port", "70000"}, "max-port"},
		{"interval too small", []string{"--interval", "10ms"}, "interval"},
		{"bad include", []string{"--include", "not-a-port"}, "include"},
		{"bad exclude", []string{"--exclude", "99999"}, "exclude"},
		{"bad remote-bind", []string{"--remote-bind", "sideways"}, "remote-bind"},
		{"conflicting host key flags", []string{"--strict-host-key", "--insecure-host-key"}, "mutually exclusive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, _, err := parse(t, append(tt.args, "devbox")...)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			_, err = cfg.Validate()
			if err == nil {
				t.Fatal("Validate should have failed")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestHostKeyPolicyMapping(t *testing.T) {
	tests := []struct {
		args []string
		want sshx.HostKeyPolicy
	}{
		{nil, ""}, // defer to ssh_config
		{[]string{"--insecure-host-key"}, sshx.HostKeyNone},
		{[]string{"--strict-host-key"}, sshx.HostKeyStrict},
		{[]string{"--accept-new-host-key"}, sshx.HostKeyAcceptNew},
	}
	for _, tt := range tests {
		cfg, _, err := parse(t, append(tt.args, "devbox")...)
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.HostKeyPolicy(); got != tt.want {
			t.Errorf("%v -> %q, want %q", tt.args, got, tt.want)
		}
	}
}

func TestRunRequiresADestination(t *testing.T) {
	cfg := Config{MinPort: 1024, MaxPort: 65535, Interval: 2 * time.Second, RemoteBind: "any"}
	err := Run(t.Context(), cfg, IO{Out: io.Discard, Err: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "destination is required") {
		t.Errorf("Run = %v, want a missing-destination error", err)
	}
}

func TestRunValidatesBeforeConnecting(t *testing.T) {
	cfg := Config{Destination: "devbox", MinPort: 9000, MaxPort: 80, Interval: time.Second, RemoteBind: "any"}
	err := Run(t.Context(), cfg, IO{Out: io.Discard, Err: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "above") {
		t.Errorf("Run = %v, want the validation error before any network access", err)
	}
}

func TestUsageMentionsTheEssentials(t *testing.T) {
	for _, want := range []string{"autotun", "destination", "--existing", "--include", "--json"} {
		if !strings.Contains(Usage, want) {
			t.Errorf("the usage text does not mention %q", want)
		}
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	d := backoffStart
	for i := 0; i < 20; i++ {
		next := nextBackoff(d)
		if next < d {
			t.Fatalf("backoff shrank from %v to %v", d, next)
		}
		d = next
	}
	if d != backoffMax {
		t.Errorf("backoff settled at %v, want the %v cap", d, backoffMax)
	}
}

func TestSleepCtxReturnsEarlyOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if sleepCtx(ctx, time.Hour) {
		t.Error("sleepCtx should report false when the context is already done")
	}
	if !sleepCtx(t.Context(), time.Millisecond) {
		t.Error("sleepCtx should report true after a completed wait")
	}
}

// The UI adapter is what makes a settings change outlive the session.
func TestUIControllerPersistsViewPrefs(t *testing.T) {
	dir := t.TempDir()
	settings := config.Open(filepath.Join(dir, config.FileName))

	mgr := tunnel.New(tunnel.NewAllocator("127.0.0.1", false), nil, tunnel.Options{
		Policy: tunnel.DefaultPolicy(),
	})
	defer mgr.Close()

	c := &uiController{Manager: mgr, settings: settings, host: "devbox"}

	if got := c.ViewPrefs(); got.ShowPreexisting || !got.InactiveLast {
		t.Errorf("defaults = %+v, want pre-existing hidden and inactive last", got)
	}

	c.SetViewPrefs(config.ViewPrefs{ShowPreexisting: true, Sort: "recent"})
	if got := c.ViewPrefs(); !got.ShowPreexisting || got.Sort != "recent" {
		t.Errorf("prefs = %+v, want them remembered", got)
	}

	// Settings are display-only: they must never start forwarding anything.
	if c.Policy().Existing {
		t.Error("a settings change altered the forwarding policy")
	}
}

// Remembered settings are display-only: a host configured to list its
// pre-existing services must still not forward them.
func TestRememberedViewDoesNotForward(t *testing.T) {
	isolatedEnv(t)

	path, err := config.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	remote := newFakeRemote(t)
	remote.echo = startEcho(t)
	port := freeLocalPort(t)
	// Present in the first scan, so only the "everything" view forwards it.
	remote.transcript = "@@AUTOTUN-READY 1 ss Linux\n@@AUTOTUN-SCAN\n" +
		fmt.Sprintf("LISTEN 0 511 127.0.0.1:%d 0.0.0.0:*\n", port) +
		"@@AUTOTUN-END\n"

	host, _, _ := net.SplitHostPort(remote.addr())
	body := fmt.Sprintf("hosts:\n  %s:\n    show_preexisting: true\n", host)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	out, _ := runApp(t, baseConfig(remote))
	waitForOutput(t, out, "connected")
	time.Sleep(700 * time.Millisecond)

	if strings.Contains(out.String(), "opened") {
		t.Errorf("listing pre-existing services must not forward them:\n%s", out.String())
	}
}
