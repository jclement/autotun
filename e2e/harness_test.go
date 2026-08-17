//go:build e2e

// Package e2e drives the real binary against a real sshd in Docker.
//
// These tests are the only place the shell prober itself is exercised, so they
// are what prove the ss, netstat and /proc paths work against genuine tool
// output rather than captured fixtures. Run them with:
//
//	mise run e2e      (or: go test -tags e2e ./e2e/...)
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	imageName   = "autotun-e2e"
	bootTimeout = 60 * time.Second
)

// buildOnce builds the Docker image and the autotun binary a single time for
// the whole suite.
var (
	buildOnce sync.Once
	binPath   string
	buildErr  error

	// containerSeq names containers uniquely across parallel tests.
	containerSeq atomic.Int64
)

func setup(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not available; skipping the e2e suite")
	}

	buildOnce.Do(func() {
		root, err := filepath.Abs("..")
		if err != nil {
			buildErr = err
			return
		}

		dir, err := os.MkdirTemp("", "autotun-e2e-bin")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "autotun")

		build := exec.Command("go", "build", "-o", binPath, ".")
		build.Dir = root
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := build.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("building autotun: %v\n%s", err, out)
			return
		}

		img := exec.Command("docker", "build", "-q", "-t", imageName, ".")
		img.Dir = filepath.Join(root, "e2e")
		if out, err := img.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("building the e2e image: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binPath
}

// remote is a running container plus the key needed to reach it.
type remote struct {
	sshPort string
	keyPath string
	name    string
}

// startRemote boots the fake dev box. hide names tools to remove, forcing the
// prober onto a fallback.
func startRemote(t *testing.T, hide string) *remote {
	t.Helper()
	setup(t) // idempotent; ensures the image exists before we try to run it

	keyDir := t.TempDir()
	keyPath := filepath.Join(keyDir, "id")
	gen := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-f", keyPath, "-C", "autotun-e2e")
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generating a test key: %v\n%s", err, out)
	}
	pub, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Fatalf("reading the generated public key: %v", err)
	}

	// A timestamp alone is not unique enough: parallel subtests can land in
	// the same nanosecond bucket on a coarse clock.
	name := fmt.Sprintf("autotun-e2e-%d-%d", os.Getpid(), containerSeq.Add(1))
	args := []string{
		"run", "--rm", "-d", "--name", name,
		"-p", "127.0.0.1::22",
		"-e", "AUTOTUN_PUBKEY=" + strings.TrimSpace(string(pub)),
	}
	if hide != "" {
		args = append(args, "-e", "AUTOTUN_HIDE="+hide)
	}
	args = append(args, imageName)

	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("starting the container: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})

	// Find the published SSH port.
	portOut, err := exec.Command("docker", "port", name, "22/tcp").Output()
	if err != nil {
		t.Fatalf("reading the published port: %v", err)
	}
	published := strings.TrimSpace(strings.Split(string(portOut), "\n")[0])
	idx := strings.LastIndex(published, ":")
	if idx < 0 {
		t.Fatalf("unexpected docker port output %q", published)
	}
	r := &remote{sshPort: published[idx+1:], keyPath: keyPath, name: name}

	waitForSSH(t, r)
	return r
}

// waitForSSH blocks until sshd is accepting connections.
func waitForSSH(t *testing.T, r *remote) {
	t.Helper()
	deadline := time.Now().Add(bootTimeout)
	for time.Now().Before(deadline) {
		cmd := exec.Command("ssh",
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "ConnectTimeout=2",
			"-o", "BatchMode=yes",
			"-i", r.keyPath,
			"-p", r.sshPort,
			"dev@127.0.0.1", "true")
		if err := cmd.Run(); err == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	logs, _ := exec.Command("docker", "logs", r.name).CombinedOutput()
	t.Fatalf("sshd never came up:\n%s", logs)
}

// execCombined runs a command and returns its combined output.
func execCombined(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// event is one line of autotun's NDJSON stream.
type event struct {
	Type       string `json:"type"`
	Event      string `json:"event"`
	State      string `json:"state"`
	Detail     string `json:"detail"`
	RemotePort int    `json:"remote_port"`
	LocalPort  int    `json:"local_port"`
	Command    string `json:"command"`
	URL        string `json:"url"`
	Reason     string `json:"reason"`
}

// session is a running autotun process and the events it has emitted.
type session struct {
	mu     sync.Mutex
	events []event
	stderr strings.Builder
	cancel context.CancelFunc
	done   chan struct{}
}

// run starts autotun against the remote in JSON mode.
func run(t *testing.T, r *remote, extra ...string) *session {
	t.Helper()
	bin := setup(t)

	args := append([]string{
		"--json",
		"--insecure-host-key",
		"-i", r.keyPath,
		"-p", r.sshPort,
		"-l", "dev",
		"--interval", "500ms",
	}, extra...)
	args = append(args, "127.0.0.1")

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin, args...)
	// Keep the run from touching the developer's real config.
	cfgDir := t.TempDir()
	cmd.Env = append(os.Environ(),
		"XDG_CONFIG_HOME="+cfgDir,
		"APPDATA="+cfgDir,
		"HOME="+t.TempDir(),
		"SSH_AUTH_SOCK=",
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("starting autotun: %v", err)
	}

	s := &session{cancel: cancel, done: make(chan struct{})}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			var ev event
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				continue
			}
			s.mu.Lock()
			s.events = append(s.events, ev)
			s.mu.Unlock()
		}
	}()
	go func() {
		b, _ := io.ReadAll(stderrPipe)
		s.mu.Lock()
		s.stderr.Write(b)
		s.mu.Unlock()
	}()
	go func() {
		_ = cmd.Wait()
		close(s.done)
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case <-s.done:
		case <-time.After(10 * time.Second):
		}
	})
	return s
}

// snapshot returns the events seen so far.
func (s *session) snapshot() []event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]event(nil), s.events...)
}

func (s *session) diagnostics() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	for _, e := range s.events {
		fmt.Fprintf(&b, "  %+v\n", e)
	}
	if s.stderr.Len() > 0 {
		fmt.Fprintf(&b, "  stderr: %s\n", s.stderr.String())
	}
	return b.String()
}

// awaitOpen waits for a tunnel to the given remote port and returns its local port.
func (s *session) awaitOpen(t *testing.T, remotePort int) int {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range s.snapshot() {
			if e.Type == "tunnel" && e.Event == "opened" && e.RemotePort == remotePort {
				return e.LocalPort
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no tunnel opened for remote port %d. events:\n%s", remotePort, s.diagnostics())
	return 0
}

// awaitStatus waits for a connection status carrying the given text.
func (s *session) awaitStatus(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range s.snapshot() {
			if e.Type == "status" && (e.State == want || strings.Contains(e.Detail, want)) {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("never saw status %q. events:\n%s", want, s.diagnostics())
}

// getPort fetches the local tunnel and returns which remote port answered.
func getPort(t *testing.T, localPort int) string {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}

	var lastErr error
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", localPort))
		if err != nil {
			lastErr = err
			time.Sleep(200 * time.Millisecond)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		return strings.TrimSpace(string(body))
	}
	t.Fatalf("could not reach the tunnel on local port %d: %v", localPort, lastErr)
	return ""
}
