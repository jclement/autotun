package sshx

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
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

// ---- Algorithm preference ----

// testECDSAKey generates a throwaway ECDSA host key, for the cases where a
// host offers a key type known_hosts has never seen.
func testECDSAKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

// record trusts key for devbox the way a first connection would, so the rest
// of the test can ask what known_hosts now holds.
func record(t *testing.T, key ssh.PublicKey) {
	t.Helper()
	cb, err := HostKeyCallback(HostKeyAcceptNew, &stubPrompter{})
	if err != nil {
		t.Fatalf("HostKeyCallback: %v", err)
	}
	if err := cb("devbox:22", remoteAddr(t), key); err != nil {
		t.Fatalf("recording a host key: %v", err)
	}
}

// The regression this whole thing exists for: a host with both an Ed25519 and
// an ECDSA key, recorded as Ed25519, must not be asked for its ECDSA key.
func TestHostKeyAlgorithmsPrefersRecordedTypes(t *testing.T) {
	isolatedHome(t)
	record(t, testKey(t)) // ed25519

	algos := HostKeyAlgorithms(HostKeyAsk, "devbox:22")
	if len(algos) == 0 || algos[0] != ssh.KeyAlgoED25519 {
		t.Fatalf("algorithms = %v, want the recorded ssh-ed25519 first", algos)
	}
	// The others stay on the list, so a genuine key rotation still negotiates
	// and is reported honestly instead of failing to agree on an algorithm.
	if !slices.Contains(algos, ssh.KeyAlgoECDSA256) {
		t.Errorf("algorithms = %v, want the remaining types kept as fallbacks", algos)
	}
}

func TestHostKeyAlgorithmsExpandsRSA(t *testing.T) {
	isolatedHome(t)
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	record(t, pub)

	algos := HostKeyAlgorithms(HostKeyAsk, "devbox:22")
	for _, want := range []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA} {
		if i := slices.Index(algos, want); i < 0 || i > 2 {
			t.Errorf("algorithms = %v, want %s among the first three", algos, want)
		}
	}
}

func TestHostKeyAlgorithmsWithNothingToPreferAreUnset(t *testing.T) {
	isolatedHome(t)
	if algos := HostKeyAlgorithms(HostKeyAsk, "devbox:22"); algos != nil {
		t.Errorf("an unknown host should express no preference, got %v", algos)
	}
	record(t, testKey(t))
	if algos := HostKeyAlgorithms(HostKeyNone, "devbox:22"); algos != nil {
		t.Errorf("verification is off, so there is nothing to prefer, got %v", algos)
	}
}

// A key type we have not recorded for a known host is a new key, not a
// changed one — ssh(1) only compares within one algorithm, and so do we.
func TestHostKeyOfANewTypeIsNotAChangedKey(t *testing.T) {
	isolatedHome(t)
	record(t, testKey(t)) // ed25519

	p := &stubPrompter{confirm: true}
	cb, err := HostKeyCallback(HostKeyAsk, p)
	if err != nil {
		t.Fatalf("HostKeyCallback: %v", err)
	}
	if err := cb("devbox:22", remoteAddr(t), testECDSAKey(t)); err != nil {
		t.Fatalf("an unrecorded key type should be offered for approval: %v", err)
	}
	if len(p.notices) == 0 || !strings.Contains(p.notices[0], "different key type") {
		t.Errorf("the user should be told the host is known by another key type: %v", p.notices)
	}
}
