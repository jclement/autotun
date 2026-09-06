// Browser bridge glue: it turns a URL as the remote host understands it into
// one that means the same thing here, and opens it. The rewriting is the
// whole point — remote 5173 is frequently local 5174, and nothing on the
// remote side is in a position to know that.
package app

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/jclement/autotun/internal/tunnel"
	"github.com/jclement/autotun/internal/ui"
)

// tunnelTable is the part of the tunnel manager the router needs.
type tunnelTable interface {
	States() []tunnel.State
	TryLocalPort(remotePort, local int) error
	ForwardNow(remotePort int) error
}

// urlRouter answers browser requests coming off the bridge.
type urlRouter struct {
	tunnels tunnelTable
	open    func(string) error
	notify  func(ui.ToastMsg)
	// canBind reports whether a local port is free, so a reclaim that cannot
	// possibly work is not attempted. Nil probes the real address.
	canBind func(port int) bool
	// bind is the local address tunnels listen on, which is where a port has
	// to be free for a reclaim to succeed.
	bind string
	// poll is how often to re-check for a tunnel that has not appeared yet.
	// A service opens its port and launches a browser in the same breath, so
	// the request usually arrives before the next probe has even run; the
	// caller's context bounds the total wait.
	poll time.Duration
}

// loopback names the hosts that mean "the machine this URL was written on".
// Anything else is a real address that means the same thing from here.
var loopback = map[string]bool{
	"localhost": true, "127.0.0.1": true, "::1": true, "0.0.0.0": true, "::": true,
}

// Open rewrites a remote URL for this machine and hands it to the browser.
func (r *urlRouter) Open(ctx context.Context, raw string) error {
	local, err := r.localize(ctx, raw)
	if err != nil {
		r.toast(ui.ToastMsg{Text: "browser: " + err.Error(), Bad: true})
		return err
	}
	if err := r.open(local); err != nil {
		r.toast(ui.ToastMsg{Text: "browser: " + err.Error(), Bad: true})
		return err
	}
	r.toast(ui.ToastMsg{Text: "opened " + local})
	return nil
}

// localize maps a URL from the remote's point of view to this one.
func (r *urlRouter) localize(ctx context.Context, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("unparsable url %q", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("refusing to open a %s url", u.Scheme)
	}
	// A public address — an OAuth consent screen, a docs link — already means
	// the same thing here, so it travels unchanged.
	if !loopback[u.Hostname()] {
		return u.String(), nil
	}

	remotePort, err := portOf(u)
	if err != nil {
		return "", err
	}
	st, ok := r.await(ctx, remotePort)
	if !ok {
		return "", fmt.Errorf("remote port %d is not forwarded", remotePort)
	}

	// A remapped port breaks any flow that told a third party where to call
	// back, so try to take the original before settling for the substitute.
	// Only when it is actually free: relocating a working tunnel to another
	// arbitrary port helps nobody and invalidates any tab already open on it.
	if st.LocalPort != remotePort && r.free(remotePort) {
		if err := r.tunnels.TryLocalPort(remotePort, remotePort); err == nil {
			if moved, ok := r.lookup(remotePort); ok {
				st = moved
			}
		}
	}
	if st.LocalPort != remotePort {
		r.toast(ui.ToastMsg{
			Text: fmt.Sprintf("remote %d is local %d; a login callback may not match", remotePort, st.LocalPort),
			Bad:  true,
		})
	}

	u.Host = net.JoinHostPort("localhost", strconv.Itoa(st.LocalPort))
	return u.String(), nil
}

// portOf reads the port a URL names, filling in the scheme's default.
func portOf(u *url.URL) (int, error) {
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return 0, fmt.Errorf("bad port %q", p)
		}
		return n, nil
	}
	if u.Scheme == "https" {
		return 443, nil
	}
	return 80, nil
}

// await returns the active tunnel for a remote port, waiting a little for the
// prober to catch up with a service that has only just started listening.
func (r *urlRouter) await(ctx context.Context, remotePort int) (tunnel.State, bool) {
	asked := false
	for {
		if st, ok := r.lookup(remotePort); ok {
			return st, true
		}
		// The port may be listed but skipped — a service that was already
		// running when autotun connected, most often. Asking to open it is
		// as explicit as a request gets, so override the policy once.
		if !asked {
			asked = true
			if err := r.tunnels.ForwardNow(remotePort); err == nil {
				continue
			}
		}
		select {
		case <-ctx.Done():
			return tunnel.State{}, false
		case <-time.After(r.poll):
		}
	}
}

// lookup finds an active tunnel by remote port.
func (r *urlRouter) lookup(remotePort int) (tunnel.State, bool) {
	for _, st := range r.tunnels.States() {
		if st.RemotePort == remotePort && st.Status == tunnel.StatusActive {
			return st, true
		}
	}
	return tunnel.State{}, false
}

// free reports whether the local port is available to move a tunnel onto.
// Something else can still take it in the moment between asking and moving,
// in which case the move fails and the tunnel keeps the port it had.
func (r *urlRouter) free(port int) bool {
	if r.canBind != nil {
		return r.canBind(port)
	}
	host := r.bind
	if host == "" {
		host = "127.0.0.1"
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

func (r *urlRouter) toast(msg ui.ToastMsg) {
	if r.notify != nil {
		r.notify(msg)
	}
}
