package browser

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// localHost stands in for an SSH connection whose far end is this machine:
// scripts run against a throwaway HOME and the socket is a real one, so the
// generated shim is exercised exactly as a dev box would run it.
type localHost struct{ home string }

func (h *localHost) Output(ctx context.Context, script string) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(os.Environ(), "HOME="+h.home, "XDG_RUNTIME_DIR=")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("%w: %s", err, ee.Stderr)
		}
		return "", err
	}
	return string(out), nil
}

func (h *localHost) ListenUnix(path string) (net.Listener, error) {
	return net.Listen("unix", path)
}

// requirePOSIX skips a test that needs this machine to stand in for the
// remote. The bridge itself is a unix socket on the far end, so a Windows
// client can use it perfectly well — but faking the remote half locally, which
// is how these tests get their coverage, needs a POSIX shell and a unix socket
// path here.
func requirePOSIX(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX shell or unix socket path to fake a remote with")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no POSIX shell to install the shim with")
	}
}

// shortHome returns a temp directory with a short path, because a unix socket
// path is capped at ~104 bytes and the usual per-test temp directory spends
// most of that budget before we start.
func shortHome(t *testing.T) string {
	t.Helper()
	requirePOSIX(t)
	home, err := os.MkdirTemp("/tmp", "autotun-browser")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	return home
}

// installed brings up a bridge over a fake remote and reports the URLs it
// receives.
func installed(t *testing.T) (*Bridge, string, <-chan string) {
	t.Helper()
	home := shortHome(t)
	urls := make(chan string, 4)

	b, err := Install(context.Background(), &localHost{home: home}, Options{
		Open: func(_ context.Context, url string) error {
			urls <- url
			return nil
		},
		Timeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b, home, urls
}

// runShim invokes the installed shim the way a program on the remote would.
func runShim(t *testing.T, path string, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{path}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running the shim: %v", err)
	}
	return stdout.String(), stderr.String(), code
}

// hasRelay reports whether this machine has any of the tools the shim can
// talk to a unix socket with.
func hasRelay(t *testing.T) bool {
	t.Helper()
	for _, tool := range []string{"nc", "socat", "python3"} {
		if _, err := exec.LookPath(tool); err == nil {
			return true
		}
	}
	return false
}

func TestShimRelaysAURL(t *testing.T) {
	b, _, urls := installed(t)
	if !hasRelay(t) {
		t.Skip("no nc, socat or python3 to reach the socket with")
	}

	const want = "http://localhost:5173/app?tab=1#top"
	stdout, stderr, code := runShim(t, b.Shim, nil, want)
	if code != 0 {
		t.Fatalf("shim exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	select {
	case got := <-urls:
		if got != want {
			t.Errorf("relayed %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the URL never arrived")
	}
}

// Everything that opens a browser on Linux goes through one of these names,
// so each has to reach the shim by a PATH lookup alone.
func TestShimIsInstalledUnderEveryOpenerName(t *testing.T) {
	_, home, urls := installed(t)
	if !hasRelay(t) {
		t.Skip("no nc, socat or python3 to reach the socket with")
	}
	bin := filepath.Join(home, ".autotun", "bin")

	for _, name := range []string{"xdg-open", "sensible-browser", "open"} {
		want := "http://localhost:3000/" + name
		cmd := exec.Command(filepath.Join(bin, name), want)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", name, err, out)
		}
		select {
		case got := <-urls:
			if got != want {
				t.Errorf("%s relayed %q, want %q", name, got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: the URL never arrived", name)
		}
	}
}

// `gio open URL` is the other spelling some desktop stacks use.
func TestShimAcceptsTheGioForm(t *testing.T) {
	b, _, urls := installed(t)
	if !hasRelay(t) {
		t.Skip("no nc, socat or python3 to reach the socket with")
	}

	const want = "https://localhost:8443/"
	if _, stderr, code := runShim(t, b.Shim, nil, "open", want); code != 0 {
		t.Fatalf("shim exited %d: %s", code, stderr)
	}
	select {
	case got := <-urls:
		if got != want {
			t.Errorf("relayed %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the URL never arrived")
	}
}

// The line in a shell rc outlives any one session, so a shim with nothing
// listening has to degrade to telling the user rather than swallowing the URL.
func TestShimWithoutASessionSaysSo(t *testing.T) {
	b, _, _ := installed(t)
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// An empty PATH stands in for a box with no browser opener of its own,
	// and stops the test from launching a real browser on one that has.
	empty := t.TempDir()
	const url = "http://localhost:5173/"
	_, stderr, code := runShim(t, b.Shim, []string{"PATH=" + empty}, url)
	if code == 0 {
		t.Error("the shim reported success with nothing listening")
	}
	if !strings.Contains(stderr, url) {
		t.Errorf("the URL should be left where the user can see it, got %q", stderr)
	}
}

func TestBridgeRejectsTheWrongNonce(t *testing.T) {
	b, _, urls := installed(t)

	reply := speak(t, b.Socket, "not-the-nonce http://localhost:5173/\n")
	if !strings.HasPrefix(reply, "err:") {
		t.Errorf("reply = %q, want a refusal", reply)
	}
	select {
	case got := <-urls:
		t.Errorf("an unauthenticated request was relayed: %q", got)
	default:
	}
}

func TestBridgeRejectsAMalformedRequest(t *testing.T) {
	b, _, _ := installed(t)
	if reply := speak(t, b.Socket, "\n"); !strings.HasPrefix(reply, "err:") {
		t.Errorf("reply = %q, want a refusal", reply)
	}
}

// A request the opener refuses comes back as an error, so the shim knows to
// fall back rather than report a browser that never opened.
func TestBridgeReportsARefusedOpen(t *testing.T) {
	home := shortHome(t)
	b, err := Install(context.Background(), &localHost{home: home}, Options{
		Open: func(context.Context, string) error { return errors.New("not forwarded") },
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	defer b.Close()

	reply := speak(t, b.Socket, b.nonce+" http://localhost:5173/\n")
	if !strings.Contains(reply, "not forwarded") {
		t.Errorf("reply = %q, want the opener's reason", reply)
	}
}

// speak sends one request to the bridge and returns its answer.
func speak(t *testing.T, socket, request string) string {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dialing the bridge: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("writing the request: %v", err)
	}
	line, _ := bufio.NewReader(conn).ReadString('\n')
	return strings.TrimSpace(line)
}

func TestSetupLinesNameNothingSessionSpecific(t *testing.T) {
	// They go in a shell rc once and have to keep working for sessions that
	// do not exist yet, so anything that changes per run is a bug.
	for _, line := range SetupLines() {
		if !strings.Contains(line, "$HOME/.autotun") {
			t.Errorf("setup line %q should be written against $HOME", line)
		}
	}
}
