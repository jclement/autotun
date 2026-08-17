//go:build e2e

package e2e

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestDiscoveryModes runs the whole stack against a real sshd with tools
// progressively removed, so every parser is exercised against genuine output.
func TestDiscoveryModes(t *testing.T) {
	modes := []struct {
		name string
		hide string
		// procMode drops process attribution expectations: without ss or
		// lsof we resolve pids through /proc socket inodes, which only works
		// for the connecting user's own processes.
		procMode bool
	}{
		{name: "ss", hide: ""},
		{name: "lsof", hide: "ss"},
		{name: "netstat", hide: "ss lsof"},
		{name: "proc", hide: "ss lsof netstat", procMode: true},
	}

	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			remote := startRemote(t, mode.hide)
			s := run(t, remote)

			// Port 3000 appears a few seconds after we connect, so it is a
			// genuinely new service, not a baseline one.
			local := s.awaitOpen(t, 3000)
			if got := getPort(t, local); got != "autotun-devserver port=3000" {
				t.Errorf("local port %d reached %q, want the server on 3000", local, got)
			}

			// And 8080 shows up later still.
			local8080 := s.awaitOpen(t, 8080)
			if got := getPort(t, local8080); got != "autotun-devserver port=8080" {
				t.Errorf("local port %d reached %q, want the server on 8080", local8080, got)
			}

			if !mode.procMode {
				assertCommand(t, s, 3000, "python3")
			}
		})
	}
}

// assertCommand checks that a tunnel was attributed to a process.
func assertCommand(t *testing.T, s *session, remotePort int, want string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range s.snapshot() {
			if e.RemotePort == remotePort && strings.Contains(e.Command, want) {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Errorf("remote port %d was never attributed to %q. events:\n%s", remotePort, want, s.diagnostics())
}

// The whole point of the default: a dev box's existing services stay untouched.
func TestPreexistingPortsAreNotForwarded(t *testing.T) {
	t.Parallel()
	remote := startRemote(t, "")
	s := run(t, remote)

	s.awaitOpen(t, 3000) // wait until we are demonstrably running
	time.Sleep(2 * time.Second)

	for _, e := range s.snapshot() {
		if e.Event == "opened" && (e.RemotePort == 5432 || e.RemotePort == 22) {
			t.Errorf("a pre-existing port (%d) was forwarded", e.RemotePort)
		}
	}
}

func TestExistingFlagForwardsTheBaseline(t *testing.T) {
	t.Parallel()
	remote := startRemote(t, "")
	s := run(t, remote, "--existing")

	local := s.awaitOpen(t, 5432)
	if got := getPort(t, local); got != "autotun-devserver port=5432" {
		t.Errorf("local port %d reached %q", local, got)
	}
}

func TestIncludeLimitsTheForwardedSet(t *testing.T) {
	t.Parallel()
	remote := startRemote(t, "")
	s := run(t, remote, "--include", "8080")

	s.awaitOpen(t, 8080)
	for _, e := range s.snapshot() {
		if e.Event == "opened" && e.RemotePort != 8080 {
			t.Errorf("--include 8080 also forwarded %d", e.RemotePort)
		}
	}
}

func TestExcludeSkipsAPort(t *testing.T) {
	t.Parallel()
	remote := startRemote(t, "")
	s := run(t, remote, "--exclude", "3000")

	s.awaitOpen(t, 8080)
	for _, e := range s.snapshot() {
		if e.Event == "opened" && e.RemotePort == 3000 {
			t.Error("an excluded port was forwarded")
		}
	}
}

// --remote-bind loopback targets exactly the services a firewall cannot help
// with: 3000 is bound to 127.0.0.1, 8080 to 0.0.0.0.
func TestRemoteBindLoopbackOnly(t *testing.T) {
	t.Parallel()
	remote := startRemote(t, "")
	s := run(t, remote, "--remote-bind", "loopback")

	s.awaitOpen(t, 3000)
	time.Sleep(3 * time.Second)

	for _, e := range s.snapshot() {
		if e.Event == "opened" && e.RemotePort == 8080 {
			t.Error("a wildcard-bound service was forwarded under --remote-bind loopback")
		}
	}
}

func TestRemappedPortIsReportedAndUsable(t *testing.T) {
	t.Parallel()

	// Hold local 3000 so the remote's 3000 cannot map straight across.
	busy, err := net.Listen("tcp", "127.0.0.1:3000")
	if err != nil {
		t.Skip("local port 3000 is already in use by something else; skipping")
	}
	defer busy.Close()

	remote := startRemote(t, "")
	s := run(t, remote)

	local := s.awaitOpen(t, 3000)
	if local == 3000 {
		t.Fatal("autotun claimed a port that was already bound")
	}
	if got := getPort(t, local); got != "autotun-devserver port=3000" {
		t.Errorf("the remapped tunnel reached %q", got)
	}
}

// A tunnel must disappear when the process behind it does.
func TestTunnelClosesWhenTheServiceStops(t *testing.T) {
	t.Parallel()
	remote := startRemote(t, "")
	s := run(t, remote)

	local := s.awaitOpen(t, 3000)
	killRemoteServer(t, remote, 3000)

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range s.snapshot() {
			if e.Type == "tunnel" && e.Event == "closed" && e.RemotePort == 3000 {
				// And the local port comes back.
				waitForFreePort(t, local)
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the tunnel was never closed. events:\n%s", s.diagnostics())
}

func TestReconnectsAfterTheLinkDrops(t *testing.T) {
	t.Parallel()
	remote := startRemote(t, "")
	s := run(t, remote)

	local := s.awaitOpen(t, 3000)

	// Kill every sshd child, dropping the connection out from under autotun.
	execInRemote(t, remote, "pkill -f 'sshd-sessio[n]' || pkill -f 'sshd: de[v]' || true")
	s.awaitStatus(t, "disconnected")

	// It should come back on its own and restore the same local port.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if portAnswers(local) {
			// And the restored tunnel must actually carry traffic again.
			if got := getPort(t, local); got != "autotun-devserver port=3000" {
				t.Errorf("the restored tunnel reached %q", got)
			}
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("the tunnel never came back on local port %d. events:\n%s", local, s.diagnostics())
}

// A link that vanishes without closing — a closed lid, a wifi handover — is the
// case a laptop actually hits. Dropping the packets rather than the connection
// means nothing tells autotun anything; only the keepalive timeout does.
func TestReconnectsAfterTheLinkIsBlackholed(t *testing.T) {
	t.Parallel()
	remote := startRemote(t, "")
	s := run(t, remote)

	local := s.awaitOpen(t, 3000)
	if !portAnswers(local) {
		t.Fatal("the tunnel never came up")
	}

	// Freezing the container leaves the TCP connection established while the
	// far end stops answering — the same shape as a sleeping laptop or a wifi
	// handover, and the case only a keepalive timeout can catch. Detaching the
	// container from its network instead would take the published port with
	// it, leaving nothing to reconnect to.
	if out, err := execCombined("docker", "pause", remote.name); err != nil {
		t.Skipf("cannot pause the container: %v\n%s", err, out)
	}
	unpaused := false
	defer func() {
		if !unpaused {
			_, _ = execCombined("docker", "unpause", remote.name)
		}
	}()

	s.awaitStatus(t, "disconnected")
	t.Log("outage noticed while the connection was still established")

	if out, err := execCombined("docker", "unpause", remote.name); err != nil {
		t.Fatalf("unpausing the container: %v\n%s", out, err)
	}
	unpaused = true

	// The container keeps its published port, so autotun should find its way
	// back and restore the same local port.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if portAnswers(local) && getPortQuiet(local) == "autotun-devserver port=3000" {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("the tunnel never came back after the blackout. events:\n%s", s.diagnostics())
}

// getPortQuiet fetches a tunnel without failing the test when it is not ready.
func getPortQuiet(port int) string {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}

func TestPlainOutputIsHumanReadable(t *testing.T) {
	t.Parallel()
	remote := startRemote(t, "")

	bin := setup(t)
	_ = bin
	s := run(t, remote)
	s.awaitOpen(t, 3000)
}

// killRemoteServer stops the dev server listening on a port inside the container.
func killRemoteServer(t *testing.T, r *remote, port int) {
	t.Helper()
	// The bracket keeps the pattern from matching the `sh -c` running it,
	// which would otherwise kill the shell instead of the target.
	execInRemote(t, r, fmt.Sprintf("pkill -f 'devserver[.]py %d' || true", port))
}

// execInRemote runs a shell command inside the container as root.
func execInRemote(t *testing.T, r *remote, script string) {
	t.Helper()
	out, err := execCombined("docker", "exec", r.name, "sh", "-c", script)
	if err != nil {
		t.Fatalf("docker exec %q: %v\n%s", script, err, out)
	}
}

func waitForFreePort(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			ln.Close()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Errorf("local port %d was never released", port)
}

func portAnswers(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
