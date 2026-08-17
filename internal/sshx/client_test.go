package sshx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// testServer is a minimal in-process SSH server. It implements just enough to
// exercise the client: `sh -s` exec sessions and direct-tcpip forwarding.
type testServer struct {
	ln      net.Listener
	cfg     *ssh.ServerConfig
	respond func(script string) string
	// echo, when set, is the address direct-tcpip channels are forwarded to.
	echo string

	mu       sync.Mutex
	scripts  []string
	forwards []string
	conns    int
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	s := &testServer{ln: ln, cfg: cfg}
	go s.serve()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *testServer) addr() string { return s.ln.Addr().String() }

func (s *testServer) serve() {
	for {
		nConn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(nConn)
	}
}

func (s *testServer) handleConn(nConn net.Conn) {
	defer nConn.Close()

	conn, chans, reqs, err := ssh.NewServerConn(nConn, s.cfg)
	if err != nil {
		return
	}
	defer conn.Close()

	s.mu.Lock()
	s.conns++
	s.mu.Unlock()

	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		switch newCh.ChannelType() {
		case "session":
			go s.handleSession(newCh)
		case "direct-tcpip":
			go s.handleDirect(newCh)
		default:
			_ = newCh.Reject(ssh.UnknownChannelType, "unsupported")
		}
	}
}

func (s *testServer) handleSession(newCh ssh.NewChannel) {
	ch, reqs, err := newCh.Accept()
	if err != nil {
		return
	}
	defer ch.Close()

	for req := range reqs {
		if req.Type != "exec" {
			_ = req.Reply(false, nil)
			continue
		}
		_ = req.Reply(true, nil)

		// The client pipes the script in on stdin; read it to EOF.
		script, _ := io.ReadAll(ch)
		s.mu.Lock()
		s.scripts = append(s.scripts, string(script))
		respond := s.respond
		s.mu.Unlock()

		if respond != nil {
			_, _ = io.WriteString(ch, respond(string(script)))
		}
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
		return
	}
}

// directTCPIP is the payload of a direct-tcpip channel open request.
type directTCPIP struct {
	DestAddr string
	DestPort uint32
	OrigAddr string
	OrigPort uint32
}

func (s *testServer) handleDirect(newCh ssh.NewChannel) {
	var payload directTCPIP
	if err := ssh.Unmarshal(newCh.ExtraData(), &payload); err != nil {
		_ = newCh.Reject(ssh.ConnectionFailed, "bad payload")
		return
	}

	s.mu.Lock()
	s.forwards = append(s.forwards, payload.DestAddr)
	echo := s.echo
	s.mu.Unlock()

	if echo == "" {
		_ = newCh.Reject(ssh.ConnectionFailed, "nothing to forward to")
		return
	}
	upstream, err := net.Dial("tcp", echo)
	if err != nil {
		_ = newCh.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := newCh.Accept()
	if err != nil {
		upstream.Close()
		return
	}
	go ssh.DiscardRequests(reqs)

	go func() { defer upstream.Close(); _, _ = io.Copy(upstream, ch) }()
	go func() { defer ch.Close(); _, _ = io.Copy(ch, upstream) }()
}

func (s *testServer) lastScript() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.scripts) == 0 {
		return ""
	}
	return s.scripts[len(s.scripts)-1]
}

func (s *testServer) forwardTargets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.forwards...)
}

// clientHome sets up an isolated HOME with a usable private key, so the client
// has an authentication method to offer and no agent to find.
func clientHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("SSH_AUTH_SOCK", "")

	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".ssh", "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
}

func connectTo(t *testing.T, s *testServer) *Client {
	t.Helper()
	host, portStr, err := net.SplitHostPort(s.addr())
	if err != nil {
		t.Fatal(err)
	}
	d, err := Resolve(host+":"+portStr, nil, Overrides{User: "tester"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	c, err := Connect(context.Background(), d, ConnectOptions{
		HostKeyPolicy: HostKeyNone,
		Timeout:       5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestClientRunPipesTheScriptAndStreamsOutput(t *testing.T) {
	clientHome(t)
	s := newTestServer(t)
	s.respond = func(script string) string { return "hello from the remote\n" }

	c := connectTo(t, s)

	var out strings.Builder
	if err := c.Run(context.Background(), "echo hi\n", &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := out.String(); got != "hello from the remote\n" {
		t.Errorf("stdout = %q", got)
	}
	if got := s.lastScript(); got != "echo hi\n" {
		t.Errorf("the remote received %q, want the script we passed", got)
	}
}

func TestClientOutputCollectsStdout(t *testing.T) {
	clientHome(t)
	s := newTestServer(t)
	s.respond = func(script string) string {
		if strings.Contains(script, "for p in") {
			return "42\t/usr/bin/node server.js\n"
		}
		return ""
	}

	c := connectTo(t, s)
	got, err := c.Output(context.Background(), "for p in 42; do :; done\n")
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if got != "42\t/usr/bin/node server.js\n" {
		t.Errorf("Output = %q", got)
	}
}

func TestClientDialForwardsThroughTheConnection(t *testing.T) {
	clientHome(t)

	// A local "remote service" the SSH server will forward to.
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()

	s := newTestServer(t)
	s.echo = echo.Addr().String()
	c := connectTo(t, s)

	conn, err := c.Dial("tcp", "127.0.0.1:3000")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "ping" {
		t.Errorf("echo = %q, want ping", buf)
	}

	// The dial address must be carried to the remote unchanged, since it is
	// what selects the service on the far side.
	if got := s.forwardTargets(); len(got) != 1 || got[0] != "127.0.0.1" {
		t.Errorf("forward targets = %v, want the requested host", got)
	}
}

func TestClientRunHonorsContextCancellation(t *testing.T) {
	clientHome(t)
	s := newTestServer(t)
	// Never respond, so the session stays open until canceled.
	s.respond = func(string) string {
		select {}
	}

	c := connectTo(t, s)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, "sleep forever\n", io.Discard) }()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestClientWaitFiresWhenTheConnectionCloses(t *testing.T) {
	clientHome(t)
	s := newTestServer(t)
	c := connectTo(t, s)

	select {
	case <-c.Wait():
		t.Fatal("Wait fired while the connection was healthy")
	default:
	}

	c.Close()
	select {
	case <-c.Wait():
	case <-time.After(5 * time.Second):
		t.Fatal("Wait never fired after Close")
	}
}

func TestClientCloseIsIdempotent(t *testing.T) {
	clientHome(t)
	s := newTestServer(t)
	c := connectTo(t, s)

	c.Close()
	c.Close() // must not panic on a double close of the wait channel
}

func TestConnectFailsWithoutAuthMethods(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("SSH_AUTH_SOCK", "")

	s := newTestServer(t)
	host, portStr, _ := net.SplitHostPort(s.addr())
	d, err := Resolve(host+":"+portStr, nil, Overrides{User: "tester"})
	if err != nil {
		t.Fatal(err)
	}

	_, err = Connect(context.Background(), d, ConnectOptions{HostKeyPolicy: HostKeyNone, Timeout: 3 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "no usable authentication") {
		t.Errorf("Connect = %v, want a no-auth-methods error", err)
	}
}

func TestConnectReportsUnreachableHosts(t *testing.T) {
	clientHome(t)

	// Bind and immediately release a port so nothing is listening on it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	host, portStr, _ := net.SplitHostPort(addr)
	d, err := Resolve(host+":"+portStr, nil, Overrides{User: "tester"})
	if err != nil {
		t.Fatal(err)
	}

	_, err = Connect(context.Background(), d, ConnectOptions{HostKeyPolicy: HostKeyNone, Timeout: 2 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "connecting to") {
		t.Errorf("Connect = %v, want a connection error naming the address", err)
	}
}
