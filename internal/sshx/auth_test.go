package sshx

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// writeKeyPair puts a private key and its .pub beside it, returning the path
// and the public half.
func writeKeyPair(t *testing.T, dir, name string) (string, ssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".pub", ssh.MarshalAuthorizedKey(sshPub), 0o644); err != nil {
		t.Fatal(err)
	}
	return path, sshPub
}

// serveAgent starts an in-process ssh-agent holding one freshly generated key,
// returning its socket path and the key's public half.
func serveAgent(t *testing.T) (string, ssh.PublicKey) {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyring := agent.NewKeyring()
	if err := keyring.Add(agent.AddedKey{PrivateKey: priv}); err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return serveKeyring(t, keyring), sshPub
}

// serveKeyringFor starts an agent holding the key stored at path.
func serveKeyringFor(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.ParseRawPrivateKey(data)
	if err != nil {
		t.Fatal(err)
	}
	keyring := agent.NewKeyring()
	if err := keyring.Add(agent.AddedKey{PrivateKey: key}); err != nil {
		t.Fatal(err)
	}
	return serveKeyring(t, keyring)
}

func serveKeyring(t *testing.T, keyring agent.Agent) string {
	t.Helper()

	// macOS caps unix socket paths near 104 bytes, so keep this short.
	dir, err := os.MkdirTemp("", "at")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = agent.ServeAgent(keyring, conn)
			}()
		}
	}()
	return sock
}

// authServer records every public key offered to it and accepts only one.
type authServer struct {
	ln net.Listener

	mu      sync.Mutex
	offered []string
}

// newAuthServer starts a server accepting only `accept`, allowing at most
// maxTries public key attempts — the same budget a real sshd enforces.
func newAuthServer(t *testing.T, accept ssh.PublicKey, maxTries int) *authServer {
	t.Helper()

	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}

	s := &authServer{}
	cfg := &ssh.ServerConfig{
		MaxAuthTries: maxTries,
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			s.mu.Lock()
			s.offered = append(s.offered, string(key.Marshal()))
			s.mu.Unlock()
			if accept != nil && bytes.Equal(key.Marshal(), accept.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, ssh.ErrNoAuth
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.ln = ln
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				sconn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				go func() {
					for ch := range chans {
						_ = ch.Reject(ssh.Prohibited, "no")
					}
				}()
				<-time.After(2 * time.Second)
				sconn.Close()
			}()
		}
	}()
	return s
}

func (s *authServer) addr() string { return s.ln.Addr().String() }

// offeredKeys returns the public keys presented, in order.
func (s *authServer) offeredKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.offered...)
}

// connectTo dials the auth server with the given destination overrides.
func (s *authServer) connect(t *testing.T, ov Overrides) error {
	t.Helper()
	host, port, _ := net.SplitHostPort(s.addr())
	d, err := Resolve(host+":"+port, nil, ov)
	if err != nil {
		t.Fatal(err)
	}
	d.User = "jeff"

	c, err := Connect(context.Background(), d, ConnectOptions{
		HostKeyPolicy: HostKeyNone,
		Timeout:       5 * time.Second,
	})
	if c != nil {
		t.Cleanup(func() { c.Close() })
	}
	return err
}

// isolatedHomeWithKey gives the test a HOME whose ~/.ssh holds one default key.
func isolatedHomeWithKey(t *testing.T) (string, ssh.PublicKey) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path, pub := writeKeyPair(t, sshDir, "id_ed25519")
	return path, pub
}

// A key held in an agent — a YubiKey, or one whose passphrase you already
// typed — is far likelier to be the live credential than a forgotten id_rsa in
// ~/.ssh. Since every refused key spends one of the server's attempts, the
// agent has to go first, or a single-attempt server rejects a working setup.
func TestAgentIsOfferedBeforeDiscoveredKeys(t *testing.T) {
	isolatedHomeWithKey(t) // a stale key on disk that the server will not accept

	sock, agentKey := serveAgent(t)
	t.Setenv("SSH_AUTH_SOCK", sock)

	s := newAuthServer(t, agentKey, 1)
	if err := s.connect(t, Overrides{}); err != nil {
		t.Fatalf("connect = %v, want the agent key to be offered first", err)
	}

	offered := s.offeredKeys()
	if len(offered) == 0 {
		t.Fatal("no keys were offered")
	}
	if offered[0] != string(agentKey.Marshal()) {
		t.Error("the first key offered was not the agent's")
	}
}

// Naming a key with -i is a statement about which one to use, so it goes first
// even when an agent is running.
func TestNamedIdentityIsOfferedBeforeTheAgent(t *testing.T) {
	sock, _ := serveAgent(t)
	t.Setenv("SSH_AUTH_SOCK", sock)

	dir := t.TempDir()
	named, namedPub := writeKeyPair(t, dir, "named")

	s := newAuthServer(t, namedPub, 1)
	if err := s.connect(t, Overrides{IdentityFiles: []string{named}}); err != nil {
		t.Fatalf("connect = %v, want the named key to be offered first", err)
	}

	offered := s.offeredKeys()
	if len(offered) == 0 || offered[0] != string(namedPub.Marshal()) {
		t.Error("the first key offered was not the one named with -i")
	}
}

// A default key the agent already holds must not be offered twice: the second
// attempt is wasted against the server's budget.
func TestDiscoveredKeysAlreadyInTheAgentAreOfferedOnce(t *testing.T) {
	path, pub := isolatedHomeWithKey(t)
	t.Setenv("SSH_AUTH_SOCK", serveKeyringFor(t, path))

	// Accept nothing, so every available key is offered and counted.
	s := newAuthServer(t, nil, 10)
	if err := s.connect(t, Overrides{}); err == nil {
		t.Fatal("connect should have failed against a server accepting nothing")
	}

	var count int
	for _, k := range s.offeredKeys() {
		if k == string(pub.Marshal()) {
			count++
		}
	}
	if count != 1 {
		t.Errorf("the key was offered %d times, want once", count)
	}
}

func TestIdentitiesOnlySuppressesTheAgent(t *testing.T) {
	sock, agentKey := serveAgent(t)
	t.Setenv("SSH_AUTH_SOCK", sock)

	dir := t.TempDir()
	named, _ := writeKeyPair(t, dir, "named")

	// The server accepts only the agent's key, which must never be offered.
	s := newAuthServer(t, agentKey, 10)
	if err := s.connect(t, Overrides{IdentityFiles: []string{named}}); err == nil {
		t.Fatal("connect should have failed with the agent suppressed")
	}

	for _, k := range s.offeredKeys() {
		if k == string(agentKey.Marshal()) {
			t.Error("an agent key was offered despite IdentitiesOnly")
		}
	}
}

func TestAuthHint(t *testing.T) {
	d := &Destination{Alias: "devbox", Host: "devbox"}

	got := authHint(errText("ssh: unable to authenticate, attempted methods [none publickey]"), d,
		[]string{"agent (2 keys)", "/home/jeff/.ssh/id_ed25519"})
	if !strings.Contains(got, "agent (2 keys)") || !strings.Contains(got, "id_ed25519") {
		t.Errorf("the hint should list what was offered:\n%s", got)
	}
	if !strings.Contains(got, "agent keys are offered first") {
		t.Errorf("the hint should explain the ordering:\n%s", got)
	}
	if !strings.Contains(got, "ssh-add -l") {
		t.Errorf("the hint should suggest checking the agent:\n%s", got)
	}
	if !strings.Contains(got, "devbox") {
		t.Errorf("the hint should name the host:\n%s", got)
	}

	d.IdentitiesOnly = true
	if got := authHint(errText("ssh: unable to authenticate"), d, nil); !strings.Contains(got, "IdentitiesOnly") {
		t.Errorf("the hint should mention IdentitiesOnly:\n%s", got)
	}

	// Unrelated failures are left alone.
	if got := authHint(errText("connection reset"), d, nil); got != "" {
		t.Errorf("hint = %q, want none for an unrelated error", got)
	}
}

type errText string

func (e errText) Error() string { return string(e) }
