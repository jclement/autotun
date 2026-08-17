package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jclement/autotun/internal/config"
	"golang.org/x/crypto/ssh"
)

// fakeRemote is an SSH server that answers the prober with a canned transcript
// and forwards direct-tcpip channels to a local echo server. It stands in for a
// remote dev box, exercising the whole stack: transport, prober, policy,
// allocation and forwarding.
type fakeRemote struct {
	ln  net.Listener
	cfg *ssh.ServerConfig

	// transcript is what the prober session emits before going quiet.
	transcript string
	// echo is where forwarded connections are sent.
	echo string

	done chan struct{}

	mu        sync.Mutex
	sessions  int
	cmdlineQs int
}

func newFakeRemote(t *testing.T) *fakeRemote {
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

	r := &fakeRemote{ln: ln, cfg: cfg, done: make(chan struct{})}
	go r.serve()
	t.Cleanup(func() {
		close(r.done)
		ln.Close()
	})
	return r
}

func (r *fakeRemote) addr() string { return r.ln.Addr().String() }

func (r *fakeRemote) serve() {
	for {
		conn, err := r.ln.Accept()
		if err != nil {
			return
		}
		go r.handle(conn)
	}
}

func (r *fakeRemote) handle(nConn net.Conn) {
	defer nConn.Close()
	conn, chans, reqs, err := ssh.NewServerConn(nConn, r.cfg)
	if err != nil {
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		switch newCh.ChannelType() {
		case "session":
			go r.session(newCh)
		case "direct-tcpip":
			go r.forward(newCh)
		default:
			_ = newCh.Reject(ssh.UnknownChannelType, "no")
		}
	}
}

func (r *fakeRemote) session(newCh ssh.NewChannel) {
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

		script, _ := io.ReadAll(ch)
		if strings.Contains(string(script), "for p in") {
			// A command-line resolution request.
			r.mu.Lock()
			r.cmdlineQs++
			r.mu.Unlock()
			_, _ = io.WriteString(ch, "4242\tnode /app/server.js\n")
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
			return
		}

		r.mu.Lock()
		r.sessions++
		r.mu.Unlock()

		_, _ = io.WriteString(ch, r.transcript)
		// Hold the session open the way the real prober loop would, so the
		// supervisor does not treat this as a dropped connection.
		<-r.done
		return
	}
}

func (r *fakeRemote) cmdlineQueries() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cmdlineQs
}

func (r *fakeRemote) forward(newCh ssh.NewChannel) {
	upstream, err := net.Dial("tcp", r.echo)
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

// startEcho runs an upper-casing echo server and returns its address.
func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 1024)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						_, _ = c.Write([]byte(strings.ToUpper(string(buf[:n]))))
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// isolatedEnv gives the test its own HOME and config directory, with an SSH key
// so the client has something to authenticate with.
//
// os.UserConfigDir reads a different variable on every platform — XDG_CONFIG_HOME
// on Linux, HOME on macOS, APPDATA on Windows — so all three are set. Missing
// one does not fail loudly: the test quietly reads and writes the developer's
// real config instead, and leaks settings into the next test.
func isolatedEnv(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	cfgDir := filepath.Join(home, ".config")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	t.Setenv("APPDATA", cfgDir)
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
	if err := os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519"), pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
}

// freeLocalPort returns a port that is free right now.
func freeLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// transcriptFor builds a prober stream: an empty baseline scan, then a scan
// carrying the given ports.
func transcriptFor(ports ...int) string {
	var b strings.Builder
	b.WriteString("@@AUTOTUN-READY 1 ss Linux\n")
	b.WriteString("@@AUTOTUN-SCAN\n@@AUTOTUN-END\n")
	// The populated scan repeats: command lines are resolved asynchronously,
	// so a later scan is what carries them into the table.
	for i := 0; i < 8; i++ {
		b.WriteString("@@AUTOTUN-SCAN\n")
		for _, p := range ports {
			fmt.Fprintf(&b, "LISTEN 0 511 127.0.0.1:%d 0.0.0.0:* users:((\"node\",pid=4242,fd=20))\n", p)
		}
		b.WriteString("@@AUTOTUN-END\n")
	}
	return b.String()
}

// syncWriter collects output safely while the app writes from its own goroutines.
type syncWriter struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// runApp starts the app in headless mode and returns its output and a stop func.
func runApp(t *testing.T, cfg Config) (*syncWriter, func()) {
	t.Helper()
	out := &syncWriter{}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, IO{Out: out, Err: io.Discard}) }()

	stop := func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Run did not return after cancellation")
		}
	}
	t.Cleanup(stop)
	return out, stop
}

// waitForOutput polls until the collected output contains want.
func waitForOutput(t *testing.T, out *syncWriter, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in output:\n%s", want, out.String())
}

// baseConfig is a headless configuration pointed at the fake remote.
func baseConfig(remote *fakeRemote) Config {
	return Config{
		Destination:    remote.addr(),
		User:           "tester",
		Bind:           "127.0.0.1",
		MinPort:        1024,
		MaxPort:        65535,
		RemoteBind:     "any",
		Interval:       300 * time.Millisecond,
		ConnectTimeout: 5 * time.Second,
		InsecureHost:   true,
		Plain:          true,
	}
}

func TestEndToEndForwardsANewPort(t *testing.T) {
	isolatedEnv(t)

	remote := newFakeRemote(t)
	remote.echo = startEcho(t)
	port := freeLocalPort(t)
	remote.transcript = transcriptFor(port)

	out, _ := runApp(t, baseConfig(remote))
	waitForOutput(t, out, "opened")

	// The tunnel should carry traffic end to end.
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatalf("dialing the forwarded port: %v", err)
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, "hello"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "HELLO" {
		t.Errorf("round trip = %q, want HELLO", buf)
	}

	// The log should name the local and remote ports and the process.
	log := out.String()
	if !strings.Contains(log, fmt.Sprint(port)) {
		t.Errorf("the log does not mention port %d:\n%s", port, log)
	}
	if !strings.Contains(log, "connected") {
		t.Errorf("the connection was never reported:\n%s", log)
	}
}

func TestEndToEndSkipsPreexistingPorts(t *testing.T) {
	isolatedEnv(t)

	remote := newFakeRemote(t)
	remote.echo = startEcho(t)
	port := freeLocalPort(t)

	// This time the port is present in the very first scan, so it is baseline.
	remote.transcript = "@@AUTOTUN-READY 1 ss Linux\n@@AUTOTUN-SCAN\n" +
		fmt.Sprintf("LISTEN 0 511 127.0.0.1:%d 0.0.0.0:*\n", port) +
		"@@AUTOTUN-END\n"

	out, _ := runApp(t, baseConfig(remote))
	waitForOutput(t, out, "connected")
	time.Sleep(700 * time.Millisecond)

	if strings.Contains(out.String(), "opened") {
		t.Errorf("a pre-existing port was forwarded:\n%s", out.String())
	}
	if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err != nil {
		t.Errorf("the pre-existing port was bound locally: %v", err)
	} else {
		ln.Close()
	}
}

func TestEndToEndExistingFlagForwardsTheBaseline(t *testing.T) {
	isolatedEnv(t)

	remote := newFakeRemote(t)
	remote.echo = startEcho(t)
	port := freeLocalPort(t)
	remote.transcript = "@@AUTOTUN-READY 1 ss Linux\n@@AUTOTUN-SCAN\n" +
		fmt.Sprintf("LISTEN 0 511 127.0.0.1:%d 0.0.0.0:*\n", port) +
		"@@AUTOTUN-END\n"

	cfg := baseConfig(remote)
	cfg.Existing = true

	out, _ := runApp(t, cfg)
	waitForOutput(t, out, "opened")
}

func TestEndToEndRespectsExclude(t *testing.T) {
	isolatedEnv(t)

	remote := newFakeRemote(t)
	remote.echo = startEcho(t)
	excluded, allowed := freeLocalPort(t), freeLocalPort(t)
	remote.transcript = transcriptFor(excluded, allowed)

	cfg := baseConfig(remote)
	cfg.Exclude = fmt.Sprint(excluded)

	out, _ := runApp(t, cfg)
	waitForOutput(t, out, fmt.Sprintf("remote %d", allowed))
	time.Sleep(500 * time.Millisecond)

	if strings.Contains(out.String(), fmt.Sprintf("remote %d", excluded)) {
		t.Errorf("an excluded port was forwarded:\n%s", out.String())
	}
}

func TestEndToEndJSONOutput(t *testing.T) {
	isolatedEnv(t)

	remote := newFakeRemote(t)
	remote.echo = startEcho(t)
	port := freeLocalPort(t)
	remote.transcript = transcriptFor(port)

	cfg := baseConfig(remote)
	cfg.Plain, cfg.JSON = false, true

	out, _ := runApp(t, cfg)
	waitForOutput(t, out, `"event":"opened"`)

	for _, want := range []string{`"type":"tunnel"`, `"remote_port":`, `"url":"http://127.0.0.1:`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the JSON stream is missing %q:\n%s", want, out.String())
		}
	}
}

func TestEndToEndResolvesCommandLines(t *testing.T) {
	isolatedEnv(t)

	remote := newFakeRemote(t)
	remote.echo = startEcho(t)
	port := freeLocalPort(t)
	remote.transcript = transcriptFor(port)

	out, _ := runApp(t, baseConfig(remote))
	waitForOutput(t, out, "opened")

	// The log records the tunnel at the moment it opens, before the
	// asynchronous lookup lands, so assert the round trip itself: autotun must
	// ask the remote for the command line exactly once per pid.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && remote.cmdlineQueries() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if n := remote.cmdlineQueries(); n != 1 {
		t.Errorf("the remote was asked for command lines %d times, want exactly 1", n)
	}
}

func TestEndToEndRemapsABusyLocalPort(t *testing.T) {
	isolatedEnv(t)

	// Hold the port the remote is using, forcing a remap.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := busy.Addr().(*net.TCPAddr).Port

	remote := newFakeRemote(t)
	remote.echo = startEcho(t)
	remote.transcript = transcriptFor(port)

	out, _ := runApp(t, baseConfig(remote))
	waitForOutput(t, out, "opened")

	// The remap marker must appear, so the different port is never a surprise.
	if !strings.Contains(out.String(), "≠") {
		t.Errorf("a remapped tunnel was not flagged:\n%s", out.String())
	}
}

func TestEndToEndSamePortReportsTheConflict(t *testing.T) {
	isolatedEnv(t)

	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := busy.Addr().(*net.TCPAddr).Port

	remote := newFakeRemote(t)
	remote.echo = startEcho(t)
	remote.transcript = transcriptFor(port)

	cfg := baseConfig(remote)
	cfg.SamePort = true

	out, _ := runApp(t, cfg)
	waitForOutput(t, out, "already in use")
}

func TestEndToEndClosesTunnelsWhenTheServiceGoesAway(t *testing.T) {
	isolatedEnv(t)

	remote := newFakeRemote(t)
	remote.echo = startEcho(t)
	port := freeLocalPort(t)

	// Appears, then vanishes for the grace period.
	remote.transcript = transcriptFor(port) +
		"@@AUTOTUN-SCAN\n@@AUTOTUN-END\n@@AUTOTUN-SCAN\n@@AUTOTUN-END\n"

	out, _ := runApp(t, baseConfig(remote))
	waitForOutput(t, out, "opened")
	waitForOutput(t, out, "closed")

	// And the local port comes back.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
			ln.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("the local port was never released")
}

func TestEndToEndReportsAnUnusableRemote(t *testing.T) {
	isolatedEnv(t)

	remote := newFakeRemote(t)
	remote.transcript = "@@AUTOTUN-ERROR no usable port discovery tool\n"

	out, _ := runApp(t, baseConfig(remote))
	waitForOutput(t, out, "no usable port discovery tool")
}

func TestEndToEndRefusesAnUnreachableHost(t *testing.T) {
	isolatedEnv(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	cfg := baseConfig(newFakeRemote(t))
	cfg.Destination = addr

	err = Run(context.Background(), cfg, IO{Out: io.Discard, Err: io.Discard})
	if err == nil {
		t.Fatal("Run should have failed against a dead address")
	}
	if !strings.Contains(err.Error(), "connecting to") {
		t.Errorf("error = %v, want it to name the connection failure", err)
	}
}

func TestEndToEndRemembersSchemesAcrossRuns(t *testing.T) {
	isolatedEnv(t)

	remote := newFakeRemote(t)
	remote.echo = startEcho(t)
	port := freeLocalPort(t)
	remote.transcript = transcriptFor(port)

	// Pre-seed the store the way the `t` key would have.
	path, err := config.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	host, _, _ := net.SplitHostPort(remote.addr())
	body := fmt.Sprintf("hosts:\n  %s:\n    ports:\n      %d:\n        scheme: https\n", host, port)
	if err = os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := baseConfig(remote)
	cfg.Plain, cfg.JSON = false, true

	out, _ := runApp(t, cfg)
	waitForOutput(t, out, `"url":"https://`)
}
