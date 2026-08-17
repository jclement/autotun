package sshx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kevinburke/ssh_config"
)

// fakeConfig is a canned ssh_config lookup table keyed by "alias/Key".
type fakeConfig map[string]string

func (f fakeConfig) Get(alias, key string) (string, error) { return f[alias+"/"+key], nil }

func TestParseTarget(t *testing.T) {
	tests := []struct {
		in   string
		user string
		host string
		port int
		err  bool
	}{
		{"devbox", "", "devbox", 0, false},
		{"jeff@devbox", "jeff", "devbox", 0, false},
		{"devbox:2222", "", "devbox", 2222, false},
		{"jeff@devbox:2222", "jeff", "devbox", 2222, false},
		{"ssh://jeff@devbox:2222", "jeff", "devbox", 2222, false},
		{"192.168.1.10", "", "192.168.1.10", 0, false},
		{"[::1]", "", "::1", 0, false},
		{"[::1]:2222", "", "::1", 2222, false},
		{"jeff@[fe80::1]:22", "jeff", "fe80::1", 22, false},
		{"fe80::1:2:3", "", "fe80::1:2:3", 0, false}, // bare v6 keeps every colon
		{"", "", "", 0, true},
		{"  ", "", "", 0, true},
		{"@devbox", "", "", 0, true},
		{"jeff@", "", "", 0, true},
		{"devbox:0", "", "", 0, true},
		{"devbox:99999", "", "", 0, true},
		{"devbox:http", "", "", 0, true},
		{"[::1:2222", "", "", 0, true},
		{"[::1]x", "", "", 0, true},
	}
	for _, tt := range tests {
		user, host, port, err := ParseTarget(tt.in)
		if tt.err {
			if err == nil {
				t.Errorf("ParseTarget(%q) should have failed", tt.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseTarget(%q) = %v", tt.in, err)
			continue
		}
		if user != tt.user || host != tt.host || port != tt.port {
			t.Errorf("ParseTarget(%q) = %q, %q, %d; want %q, %q, %d",
				tt.in, user, host, port, tt.user, tt.host, tt.port)
		}
	}
}

func TestResolveUsesSSHConfig(t *testing.T) {
	cfg := fakeConfig{
		"devbox/HostName":     "10.0.0.7",
		"devbox/User":         "jeff",
		"devbox/Port":         "2222",
		"devbox/IdentityFile": "/keys/devbox_ed25519",
		"devbox/ProxyJump":    "bastion",
	}
	d, err := Resolve("devbox", cfg, Overrides{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if d.Host != "10.0.0.7" || d.User != "jeff" || d.Port != 2222 {
		t.Errorf("got %+v, want 10.0.0.7 / jeff / 2222", d)
	}
	if d.ProxyJump != "bastion" {
		t.Errorf("ProxyJump = %q, want bastion", d.ProxyJump)
	}
	if len(d.IdentityFiles) != 1 || d.IdentityFiles[0] != "/keys/devbox_ed25519" {
		t.Errorf("IdentityFiles = %v", d.IdentityFiles)
	}
	// The alias is preserved for display even after HostName rewriting.
	if d.Alias != "devbox" || d.Label() != "devbox" {
		t.Errorf("Alias/Label = %q/%q, want devbox", d.Alias, d.Label())
	}
}

func TestResolveCommandLineWinsOverConfig(t *testing.T) {
	cfg := fakeConfig{
		"devbox/HostName":     "10.0.0.7",
		"devbox/User":         "jeff",
		"devbox/Port":         "2222",
		"devbox/IdentityFile": "/keys/from_config",
		"devbox/ProxyJump":    "bastion",
	}
	d, err := Resolve("devbox", cfg, Overrides{
		User:          "root",
		Port:          22,
		IdentityFiles: []string{"/keys/explicit"},
		ProxyJump:     "other-bastion",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if d.User != "root" || d.Port != 22 || d.ProxyJump != "other-bastion" {
		t.Errorf("overrides not applied: %+v", d)
	}
	// -i replaces rather than appends, matching ssh(1).
	if len(d.IdentityFiles) != 1 || d.IdentityFiles[0] != "/keys/explicit" {
		t.Errorf("IdentityFiles = %v, want only the explicit key", d.IdentityFiles)
	}
}

func TestResolveTypedValuesWinOverConfig(t *testing.T) {
	cfg := fakeConfig{"devbox/User": "jeff", "devbox/Port": "2222"}
	d, err := Resolve("root@devbox:2200", cfg, Overrides{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if d.User != "root" || d.Port != 2200 {
		t.Errorf("got %s@%d, want root@2200", d.User, d.Port)
	}
}

func TestResolveDefaults(t *testing.T) {
	d, err := Resolve("plainhost", nil, Overrides{User: "someone"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if d.Host != "plainhost" || d.Port != 22 {
		t.Errorf("got %s:%d, want plainhost:22", d.Host, d.Port)
	}
	if d.Addr() != "plainhost:22" {
		t.Errorf("Addr() = %q", d.Addr())
	}
}

func TestResolveExpandsHostNameToken(t *testing.T) {
	cfg := fakeConfig{"web1/HostName": "%h.internal.example.com"}
	d, err := Resolve("web1", cfg, Overrides{User: "u"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if d.Host != "web1.internal.example.com" {
		t.Errorf("Host = %q, want the %%h expansion", d.Host)
	}
}

func TestResolveIgnoresProxyJumpNone(t *testing.T) {
	cfg := fakeConfig{"devbox/ProxyJump": "none"}
	d, err := Resolve("devbox", cfg, Overrides{User: "u"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if d.ProxyJump != "" {
		t.Errorf("ProxyJump = %q, want empty for \"none\"", d.ProxyJump)
	}
}

func TestResolveReadsStrictHostKeyChecking(t *testing.T) {
	cfg := fakeConfig{"devbox/StrictHostKeyChecking": "accept-new"}
	d, err := Resolve("devbox", cfg, Overrides{User: "u"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if ParseHostKeyPolicy(d.StrictHostKey) != HostKeyAcceptNew {
		t.Errorf("StrictHostKey = %q", d.StrictHostKey)
	}
}

func TestResolveRejectsBadDestination(t *testing.T) {
	if _, err := Resolve("", nil, Overrides{}); err == nil {
		t.Error("want an error for an empty destination")
	}
}

func TestDestinationString(t *testing.T) {
	tests := []struct {
		d    Destination
		want string
	}{
		{Destination{Host: "devbox", User: "jeff", Port: 22}, "jeff@devbox"},
		{Destination{Host: "devbox", User: "jeff", Port: 2222}, "jeff@devbox:2222"},
		{Destination{Host: "devbox", Port: 22}, "devbox"},
	}
	for _, tt := range tests {
		if got := tt.d.String(); got != tt.want {
			t.Errorf("String() = %q, want %q", got, tt.want)
		}
	}
}

func TestDestinationAddrBracketsIPv6(t *testing.T) {
	d := Destination{Host: "::1", Port: 2222}
	if got := d.Addr(); got != "[::1]:2222" {
		t.Errorf("Addr() = %q, want [::1]:2222", got)
	}
	// An already-bracketed host must not be double-bracketed.
	d = Destination{Host: "[::1]", Port: 22}
	if got := d.Addr(); got != "[::1]:22" {
		t.Errorf("Addr() = %q, want [::1]:22", got)
	}
}

func TestDestinationLabelFallsBackToHost(t *testing.T) {
	d := Destination{Alias: "devbox", Host: "devbox"}
	if got := d.Label(); got != "devbox" {
		t.Errorf("Label() = %q", got)
	}
	d = Destination{Alias: "", Host: "10.0.0.1"}
	if got := d.Label(); got != "10.0.0.1" {
		t.Errorf("Label() = %q", got)
	}
}

func TestExpandPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	if got := ExpandPath("~/.ssh/id_ed25519"); got != filepath.Join(home, ".ssh", "id_ed25519") {
		t.Errorf("ExpandPath(~) = %q", got)
	}
	if got := ExpandPath("/absolute/path"); got != "/absolute/path" {
		t.Errorf("ExpandPath(absolute) = %q", got)
	}
	if got := ExpandPath(`"/quoted/path"`); got != "/quoted/path" {
		t.Errorf("ExpandPath(quoted) = %q", got)
	}
	t.Setenv("AUTOTUN_TEST_DIR", "/expanded")
	if got := ExpandPath("$AUTOTUN_TEST_DIR/key"); got != "/expanded/key" {
		t.Errorf("ExpandPath(env) = %q", got)
	}
}

func TestParseJumpChain(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"bastion", []string{"bastion"}},
		{"a,b,c", []string{"a", "b", "c"}},
		{" a , b ", []string{"a", "b"}},
		{",,", nil},
	}
	for _, tt := range tests {
		got := parseJumpChain(tt.in)
		if len(got) != len(tt.want) {
			t.Errorf("parseJumpChain(%q) = %v, want %v", tt.in, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("parseJumpChain(%q) = %v, want %v", tt.in, got, tt.want)
				break
			}
		}
	}
}

// chainConfig must consult files in order and let the first non-empty answer win.
func TestChainConfigFirstMatchWins(t *testing.T) {
	user := decode(t, "Host devbox\n  User first\n")
	system := decode(t, "Host devbox\n  User second\n  Port 2222\n")
	chain := chainConfig{user, system}

	if got, _ := chain.Get("devbox", "User"); got != "first" {
		t.Errorf("User = %q, want the user config's value", got)
	}
	if got, _ := chain.Get("devbox", "Port"); got != "2222" {
		t.Errorf("Port = %q, want the system config's value as a fallback", got)
	}
	if got, _ := chain.Get("devbox", "Nonexistent"); got != "" {
		t.Errorf("Nonexistent = %q, want empty", got)
	}
}

func decode(t *testing.T, s string) *ssh_config.Config {
	t.Helper()
	cfg, err := ssh_config.Decode(strings.NewReader(s))
	if err != nil {
		t.Fatalf("decoding ssh_config: %v", err)
	}
	return cfg
}

func TestLoadConfigReadsTheUserFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "Host devbox\n  HostName 10.1.2.3\n  User jeff\n"
	if err := os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	d, err := Resolve("devbox", cfg, Overrides{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if d.Host != "10.1.2.3" || d.User != "jeff" {
		t.Errorf("got %+v, want 10.1.2.3 / jeff", d)
	}
}

func TestLoadConfigToleratesAMissingFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	if _, err := LoadConfig(); err != nil {
		t.Errorf("LoadConfig with no ~/.ssh/config = %v, want nil", err)
	}
}

func TestParseHostKeyPolicy(t *testing.T) {
	tests := map[string]HostKeyPolicy{
		"yes":        HostKeyStrict,
		"YES":        HostKeyStrict,
		"no":         HostKeyNone,
		"off":        HostKeyNone,
		"accept-new": HostKeyAcceptNew,
		"ask":        HostKeyAsk,
		"":           HostKeyAsk,
		"nonsense":   HostKeyAsk,
	}
	for in, want := range tests {
		if got := ParseHostKeyPolicy(in); got != want {
			t.Errorf("ParseHostKeyPolicy(%q) = %q, want %q", in, got, want)
		}
	}
}
