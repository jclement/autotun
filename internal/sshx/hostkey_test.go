package sshx

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// testKey generates a throwaway host key.
func testKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = priv
	signer, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// stubPrompter answers questions from a script.
type stubPrompter struct {
	confirm    bool
	confirmErr error
	secret     string
	line       string
	notices    []string
	asked      []string
}

func (p *stubPrompter) Confirm(q string) (bool, error) {
	p.asked = append(p.asked, q)
	return p.confirm, p.confirmErr
}
func (p *stubPrompter) Secret(q string) (string, error) {
	p.asked = append(p.asked, q)
	return p.secret, nil
}
func (p *stubPrompter) Line(q string) (string, error) {
	p.asked = append(p.asked, q)
	return p.line, nil
}
func (p *stubPrompter) Notice(m string) { p.notices = append(p.notices, m) }

// isolatedHome points HOME at a fresh directory with an empty known_hosts.
func isolatedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	return home
}

func remoteAddr(t *testing.T) net.Addr {
	t.Helper()
	addr, err := net.ResolveTCPAddr("tcp", "10.0.0.7:22")
	if err != nil {
		t.Fatal(err)
	}
	return addr
}

func TestHostKeyNoneSkipsVerification(t *testing.T) {
	cb, err := HostKeyCallback(HostKeyNone, nil)
	if err != nil {
		t.Fatalf("HostKeyCallback: %v", err)
	}
	if err := cb("devbox:22", remoteAddr(t), testKey(t)); err != nil {
		t.Errorf("insecure callback rejected a key: %v", err)
	}
}

func TestHostKeyAskAcceptsAndRecords(t *testing.T) {
	home := isolatedHome(t)
	p := &stubPrompter{confirm: true}

	cb, err := HostKeyCallback(HostKeyAsk, p)
	if err != nil {
		t.Fatalf("HostKeyCallback: %v", err)
	}
	key := testKey(t)
	if err := cb("devbox:22", remoteAddr(t), key); err != nil {
		t.Fatalf("callback rejected an accepted key: %v", err)
	}
	if len(p.asked) == 0 {
		t.Error("the user was never asked")
	}
	if len(p.notices) == 0 || !strings.Contains(p.notices[0], ssh.FingerprintSHA256(key)) {
		t.Errorf("the fingerprint was not shown: %v", p.notices)
	}

	// The key must be written to known_hosts, so the next connection is silent.
	data, err := os.ReadFile(filepath.Join(home, ".ssh", "known_hosts"))
	if err != nil {
		t.Fatalf("reading known_hosts: %v", err)
	}
	if !strings.Contains(string(data), string(ssh.MarshalAuthorizedKey(key))[:40]) {
		t.Errorf("known_hosts does not contain the accepted key:\n%s", data)
	}

	cb2, err := HostKeyCallback(HostKeyAsk, &stubPrompter{confirm: false})
	if err != nil {
		t.Fatalf("HostKeyCallback: %v", err)
	}
	if err := cb2("devbox:22", remoteAddr(t), key); err != nil {
		t.Errorf("a recorded key should verify without asking again: %v", err)
	}
}

func TestHostKeyAskRejection(t *testing.T) {
	isolatedHome(t)
	cb, err := HostKeyCallback(HostKeyAsk, &stubPrompter{confirm: false})
	if err != nil {
		t.Fatalf("HostKeyCallback: %v", err)
	}
	err = cb("devbox:22", remoteAddr(t), testKey(t))
	if !errors.Is(err, ErrHostKeyRejected) {
		t.Errorf("err = %v, want ErrHostKeyRejected", err)
	}
}

func TestHostKeyAskWithoutAPrompter(t *testing.T) {
	isolatedHome(t)
	cb, err := HostKeyCallback(HostKeyAsk, nil)
	if err != nil {
		t.Fatalf("HostKeyCallback: %v", err)
	}
	err = cb("devbox:22", remoteAddr(t), testKey(t))
	if err == nil || !strings.Contains(err.Error(), "no terminal") {
		t.Errorf("err = %v, want a no-terminal error", err)
	}
}

func TestHostKeyStrictRefusesUnknownHosts(t *testing.T) {
	isolatedHome(t)
	cb, err := HostKeyCallback(HostKeyStrict, &stubPrompter{confirm: true})
	if err != nil {
		t.Fatalf("HostKeyCallback: %v", err)
	}
	err = cb("devbox:22", remoteAddr(t), testKey(t))
	if err == nil || !strings.Contains(err.Error(), "not in known_hosts") {
		t.Errorf("err = %v, want a strict-mode rejection", err)
	}
}

func TestHostKeyAcceptNewDoesNotAsk(t *testing.T) {
	home := isolatedHome(t)
	p := &stubPrompter{}

	cb, err := HostKeyCallback(HostKeyAcceptNew, p)
	if err != nil {
		t.Fatalf("HostKeyCallback: %v", err)
	}
	if err := cb("devbox:22", remoteAddr(t), testKey(t)); err != nil {
		t.Fatalf("accept-new rejected an unknown host: %v", err)
	}
	if len(p.asked) != 0 {
		t.Errorf("accept-new should not prompt, but asked %v", p.asked)
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh", "known_hosts")); err != nil {
		t.Errorf("known_hosts was not written: %v", err)
	}
}

// A key that changed is never auto-accepted, in any policy short of "no".
func TestHostKeyChangedIsAlwaysRefused(t *testing.T) {
	isolatedHome(t)
	original := testKey(t)

	cb, err := HostKeyCallback(HostKeyAcceptNew, &stubPrompter{})
	if err != nil {
		t.Fatalf("HostKeyCallback: %v", err)
	}
	if err := cb("devbox:22", remoteAddr(t), original); err != nil {
		t.Fatalf("recording the first key: %v", err)
	}

	for _, policy := range []HostKeyPolicy{HostKeyAcceptNew, HostKeyAsk, HostKeyStrict} {
		cb, err := HostKeyCallback(policy, &stubPrompter{confirm: true})
		if err != nil {
			t.Fatalf("HostKeyCallback(%q): %v", policy, err)
		}
		err = cb("devbox:22", remoteAddr(t), testKey(t)) // a different key
		if err == nil {
			t.Errorf("policy %q accepted a changed host key", policy)
			continue
		}
		if !strings.Contains(err.Error(), "IDENTIFICATION HAS CHANGED") {
			t.Errorf("policy %q error = %v, want the changed-key warning", policy, err)
		}
		if !strings.Contains(err.Error(), "ssh-keygen -R") {
			t.Errorf("policy %q error should tell the user how to fix it: %v", policy, err)
		}
	}
}

func TestKnownHostsFileIsCreated(t *testing.T) {
	home := isolatedHome(t)
	_ = os.Remove(filepath.Join(home, ".ssh", "known_hosts"))

	if _, err := HostKeyCallback(HostKeyAsk, &stubPrompter{}); err != nil {
		t.Fatalf("HostKeyCallback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh", "known_hosts")); err != nil {
		t.Errorf("known_hosts should have been created: %v", err)
	}
}
