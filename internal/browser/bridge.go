// Package browser relays "open this in a browser" requests from the remote
// host back to the machine you are sitting at.
//
// The remote half is a shell shim that autotun writes into the user's home
// directory and a unix socket reverse-forwarded over the existing SSH
// connection. Anything on the remote that honors $BROWSER, or that runs
// xdg-open, hands the URL to the shim; the shim writes it down the socket;
// autotun rewrites the port through its live tunnel table and opens the
// result locally. The socket is the security boundary: it lives in the
// remote user's own runtime directory with 0600 permissions, so reaching it
// already means being that user on that box.
package browser

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	_ "embed"
)

//go:embed shim.sh
var shimScript string

// Host is the part of an SSH connection the bridge needs: run a command, and
// listen on a unix socket at the far end.
type Host interface {
	Output(ctx context.Context, script string) (string, error)
	ListenUnix(path string) (net.Listener, error)
}

// Options configures a bridge.
type Options struct {
	// Open is called for each URL the remote asks for. Returning an error
	// tells the shim to fall back to whatever the remote would have done on
	// its own, so it should be reserved for URLs that genuinely cannot be
	// opened here.
	Open func(ctx context.Context, rawURL string) error
	// Timeout bounds one request, including any wait for a tunnel to appear.
	Timeout time.Duration
}

// Bridge is a live browser relay. Close it when the connection it was
// installed over goes away; the next connection gets a new one.
type Bridge struct {
	// Socket is the remote path the shim writes to.
	Socket string
	// Shim is the remote path of the script itself.
	Shim string

	opts     Options
	listener net.Listener
	nonce    string

	closeOnce sync.Once
	done      chan struct{}
	wg        sync.WaitGroup
}

// Remote paths. They are fixed rather than per-session because the line the
// user puts in their shell rc has to keep working across every future
// session, including ones that have not started yet.
const (
	remoteDir  = "$HOME/.autotun"
	shimName   = "open"
	binName    = "bin"
	socketName = "autotun-browser.sock"
)

// defaultTimeout bounds a request. It is generous because most of it is spent
// waiting for the prober to notice the port the URL refers to, which happens
// on the probe interval.
const defaultTimeout = 6 * time.Second

// maxRequest caps a request line. A URL that long is a mistake or an attack,
// either way not something to allocate for.
const maxRequest = 8 << 10

// openerNames are the programs the shim stands in for, symlinked into the
// shim's bin directory so a PATH lookup finds it first.
var openerNames = []string{"xdg-open", "sensible-browser", "x-www-browser", "www-browser", "open", "gio"}

// SetupLines are the shell rc lines that point a remote shell at the shim.
// They name no session-specific value on purpose: paste them into .zshrc once
// and every later autotun session is picked up automatically.
func SetupLines() []string {
	return []string{
		`export BROWSER="$HOME/.autotun/open"`,
		`export PATH="$HOME/.autotun/bin:$PATH"`,
	}
}

// Install writes the shim to the remote host and starts listening for it.
func Install(ctx context.Context, h Host, opts Options) (*Bridge, error) {
	if opts.Open == nil {
		return nil, errors.New("browser: an Open function is required")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}

	paths, err := prepareRemote(ctx, h)
	if err != nil {
		return nil, err
	}

	// The socket has to exist before it can be locked down, and only sshd can
	// create it, so bind first and fix the permissions immediately after.
	ln, err := h.ListenUnix(paths.socket)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", paths.socket, err)
	}

	nonce, err := newNonce()
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	if err := writeShim(ctx, h, paths, nonce); err != nil {
		_ = ln.Close()
		return nil, err
	}

	b := &Bridge{
		Socket:   paths.socket,
		Shim:     paths.shim,
		opts:     opts,
		listener: ln,
		nonce:    nonce,
		done:     make(chan struct{}),
	}
	b.wg.Add(1)
	go b.serve()
	return b, nil
}

// remotePaths are the absolute paths on the remote host, resolved once
// because $HOME and $XDG_RUNTIME_DIR are the remote's business, not ours.
type remotePaths struct {
	shim   string
	bin    string
	socket string
}

// prepareRemote creates the directories, clears any socket a previous session
// left behind, and reports where everything landed.
func prepareRemote(ctx context.Context, h Host) (remotePaths, error) {
	script := fmt.Sprintf(`
set -e
dir=${XDG_RUNTIME_DIR:-}
if [ -z "$dir" ] || [ ! -d "$dir" ] || [ ! -w "$dir" ]; then
	dir=%[1]s
fi
mkdir -p %[1]s/%[2]s "$dir"
rm -f "$dir/%[3]s"
printf '%%s\n%%s\n%%s\n' %[1]s/%[4]s %[1]s/%[2]s "$dir/%[3]s"
`, remoteDir, binName, socketName, shimName)

	out, err := h.Output(ctx, script)
	if err != nil {
		return remotePaths{}, fmt.Errorf("preparing the remote browser shim: %w", err)
	}
	lines := strings.Fields(out)
	if len(lines) != 3 {
		return remotePaths{}, fmt.Errorf("preparing the remote browser shim: unexpected output %q", out)
	}
	return remotePaths{shim: lines[0], bin: lines[1], socket: lines[2]}, nil
}

// writeShim installs the script and the opener symlinks that shadow the real
// ones on PATH, and locks down the socket sshd has just created.
func writeShim(ctx context.Context, h Host, paths remotePaths, nonce string) error {
	shim := strings.NewReplacer(
		"@SOCKET@", paths.socket,
		"@NONCE@", nonce,
		"@BINDIR@", paths.bin,
	).Replace(shimScript)

	script := fmt.Sprintf(`
set -e
cat > '%[1]s' <<'AUTOTUN_SHIM_EOF'
%[2]s
AUTOTUN_SHIM_EOF
chmod 700 '%[1]s'
chmod 600 '%[3]s' 2>/dev/null || true
for name in %[4]s; do
	ln -sf '%[1]s' '%[5]s'/"$name"
done
`, paths.shim, shim, paths.socket, strings.Join(openerNames, " "), paths.bin)

	if _, err := h.Output(ctx, script); err != nil {
		return fmt.Errorf("writing the remote browser shim: %w", err)
	}
	return nil
}

// newNonce returns the shared secret the shim proves it has read. The socket
// permissions are the real defense; this is what stops a socket accidentally
// left readable from being usable.
func newNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating a browser bridge nonce: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// serve accepts shim connections until the bridge is closed.
func (b *Bridge) serve() {
	defer b.wg.Done()
	for {
		conn, err := b.listener.Accept()
		if err != nil {
			select {
			case <-b.done:
			default:
			}
			return
		}
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.handle(conn)
		}()
	}
}

// handle reads one request and answers it. The answer matters: "ok" means the
// URL is on its way to a browser here, and anything else tells the shim to
// fall back rather than leave the user staring at a command that silently did
// nothing.
func (b *Bridge) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(b.opts.Timeout + time.Second))

	line, err := bufio.NewReader(io.LimitReader(conn, maxRequest)).ReadString('\n')
	if err != nil && line == "" {
		return
	}

	url, err := b.parse(line)
	if err != nil {
		fmt.Fprintf(conn, "err: %s\n", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), b.opts.Timeout)
	defer cancel()
	if err := b.opts.Open(ctx, url); err != nil {
		fmt.Fprintf(conn, "err: %s\n", err)
		return
	}
	fmt.Fprintln(conn, "ok")
}

// parse splits and authenticates a request line.
func (b *Bridge) parse(line string) (string, error) {
	nonce, url, ok := strings.Cut(strings.TrimSpace(line), " ")
	if !ok {
		return "", errors.New("malformed request")
	}
	if subtle.ConstantTimeCompare([]byte(nonce), []byte(b.nonce)) != 1 {
		return "", errors.New("stale shim: reinstall it by restarting autotun")
	}
	if url = strings.TrimSpace(url); url == "" {
		return "", errors.New("empty url")
	}
	return url, nil
}

// Close stops listening. The shim it wrote stays behind on purpose: it is
// what the user's shell rc points at, and it copes with nothing listening.
func (b *Bridge) Close() error {
	var err error
	b.closeOnce.Do(func() {
		close(b.done)
		err = b.listener.Close()
	})
	b.wg.Wait()
	return err
}
