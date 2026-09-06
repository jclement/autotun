package app

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jclement/autotun/internal/tunnel"
	"github.com/jclement/autotun/internal/ui"
)

// fakeTunnels is a tunnel table with no tunnels behind it. The real manager
// is safe to read while the prober writes to it, so this has to be too.
type fakeTunnels struct {
	mu      sync.Mutex
	states  []tunnel.State
	pinned  []int
	pinFail bool
	// skipped is a port the prober has listed but policy is not forwarding;
	// asking to forward it turns it into an active tunnel.
	skipped     int
	forwardFail bool
	forwarded   []int
}

func (f *fakeTunnels) States() []tunnel.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tunnel.State(nil), f.states...)
}

func (f *fakeTunnels) setStates(states ...tunnel.State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states = states
}

func (f *fakeTunnels) ForwardNow(remotePort int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forwarded = append(f.forwarded, remotePort)
	if f.forwardFail || remotePort != f.skipped {
		return errStub
	}
	f.states = append(f.states, active(remotePort, remotePort))
	return nil
}

func (f *fakeTunnels) TryLocalPort(remotePort, local int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pinned = append(f.pinned, remotePort)
	if f.pinFail {
		return errStub
	}
	for i := range f.states {
		if f.states[i].RemotePort == remotePort {
			f.states[i].LocalPort = local
		}
	}
	return nil
}

var errStub = stubError("local port is in use")

type stubError string

func (e stubError) Error() string { return string(e) }

// active builds a forwarded row.
func active(remote, local int) tunnel.State {
	return tunnel.State{RemotePort: remote, LocalPort: local, Status: tunnel.StatusActive}
}

// router wires a router to a fake table, reporting what it opened and said.
func router(t *testing.T, tt *fakeTunnels) (*urlRouter, *[]string, *[]ui.ToastMsg) {
	t.Helper()
	var opened []string
	var toasts []ui.ToastMsg
	r := &urlRouter{
		tunnels: tt,
		open:    func(url string) error { opened = append(opened, url); return nil },
		notify:  func(m ui.ToastMsg) { toasts = append(toasts, m) },
		canBind: func(int) bool { return true },
		poll:    time.Millisecond,
	}
	return r, &opened, &toasts
}

func TestRouterRewritesToTheLocalPort(t *testing.T) {
	tt := &fakeTunnels{states: []tunnel.State{active(5173, 5174)}}
	r, opened, _ := router(t, tt)

	if err := r.Open(context.Background(), "http://localhost:5173/app?tab=1#top"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	// The port moves; everything the URL was actually pointing at does not.
	want := "http://localhost:5173/app?tab=1#top"
	if len(*opened) != 1 || (*opened)[0] != want {
		t.Fatalf("opened %v, want %q", *opened, want)
	}
	// 5174 only exists because 5173 was busy when the tunnel came up, so the
	// original is worth reclaiming before a callback URL is sent anywhere.
	if len(tt.pinned) != 1 || tt.pinned[0] != 5173 {
		t.Errorf("pinned %v, want a single attempt on 5173", tt.pinned)
	}
}

func TestRouterKeepsTheRemappedPortWhenItCannotReclaim(t *testing.T) {
	tt := &fakeTunnels{states: []tunnel.State{active(8976, 8977)}, pinFail: true}
	r, opened, toasts := router(t, tt)

	if err := r.Open(context.Background(), "http://localhost:8976/oauth/callback"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	want := "http://localhost:8977/oauth/callback"
	if len(*opened) != 1 || (*opened)[0] != want {
		t.Fatalf("opened %v, want %q", *opened, want)
	}
	// A login flow that is about to fail should say why before it does.
	if !hasToast(*toasts, "may not match") {
		t.Errorf("toasts = %v, want a warning about the remapped port", *toasts)
	}
}

func TestRouterOpensAnAlreadyLocalPortUnchanged(t *testing.T) {
	tt := &fakeTunnels{states: []tunnel.State{active(3000, 3000)}}
	r, opened, _ := router(t, tt)

	if err := r.Open(context.Background(), "http://127.0.0.1:3000/"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(*opened) != 1 || (*opened)[0] != "http://localhost:3000/" {
		t.Fatalf("opened %v", *opened)
	}
	if len(tt.pinned) != 0 {
		t.Errorf("nothing needed relocating, but pinned %v", tt.pinned)
	}
}

// A public URL — a login screen, a docs page — already means the same thing
// from here, so it travels untouched.
func TestRouterPassesExternalURLsThrough(t *testing.T) {
	r, opened, _ := router(t, &fakeTunnels{})

	const want = "https://github.com/login/device"
	if err := r.Open(context.Background(), want); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(*opened) != 1 || (*opened)[0] != want {
		t.Fatalf("opened %v, want %q", *opened, want)
	}
}

// Opening an unforwarded remote port locally would silently point the browser
// at whatever this machine happens to be running there, which is worse than
// not opening it at all.
func TestRouterRefusesAPortItIsNotForwarding(t *testing.T) {
	r, opened, toasts := router(t, &fakeTunnels{})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := r.Open(ctx, "http://localhost:9999/"); err == nil {
		t.Fatal("an unforwarded port was opened")
	}
	if len(*opened) != 0 {
		t.Errorf("opened %v", *opened)
	}
	if !hasToast(*toasts, "not forwarded") {
		t.Errorf("toasts = %v, want an explanation", *toasts)
	}
}

// The prober runs on an interval, so a service that opens its port and calls
// the browser in the same breath asks before autotun has caught up.
func TestRouterWaitsForATunnelToAppear(t *testing.T) {
	tt := &fakeTunnels{}
	r, opened, _ := router(t, tt)
	go func() {
		time.Sleep(20 * time.Millisecond)
		tt.setStates(active(4321, 4321))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Open(ctx, "http://localhost:4321/"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(*opened) != 1 || (*opened)[0] != "http://localhost:4321/" {
		t.Fatalf("opened %v", *opened)
	}
}

func TestRouterRefusesNonHTTPSchemes(t *testing.T) {
	r, opened, _ := router(t, &fakeTunnels{})
	for _, raw := range []string{"file:///etc/passwd", "ssh://localhost:22", "javascript:alert(1)"} {
		if err := r.Open(context.Background(), raw); err == nil {
			t.Errorf("%s was accepted", raw)
		}
	}
	if len(*opened) != 0 {
		t.Errorf("opened %v", *opened)
	}
}

func TestRouterFillsInTheSchemeDefaultPort(t *testing.T) {
	tt := &fakeTunnels{states: []tunnel.State{active(80, 8080)}}
	r, opened, _ := router(t, tt)

	if err := r.Open(context.Background(), "http://localhost/health"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(*opened) != 1 || !strings.HasSuffix((*opened)[0], ":80/health") {
		t.Fatalf("opened %v", *opened)
	}
}

func hasToast(toasts []ui.ToastMsg, want string) bool {
	for _, m := range toasts {
		if strings.Contains(m.Text, want) {
			return true
		}
	}
	return false
}

// A dev server that was already running when autotun connected is skipped by
// policy, but naming it in a browser request is an explicit instruction.
func TestRouterForwardsASkippedPortOnRequest(t *testing.T) {
	tt := &fakeTunnels{skipped: 5173}
	r, opened, _ := router(t, tt)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Open(ctx, "http://localhost:5173/"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(*opened) != 1 || (*opened)[0] != "http://localhost:5173/" {
		t.Fatalf("opened %v", *opened)
	}
	if len(tt.forwarded) != 1 || tt.forwarded[0] != 5173 {
		t.Errorf("forwarded %v, want a single request for 5173", tt.forwarded)
	}
}

// A port the user switched off stays off: that decision was about this port,
// not a blanket policy the request should outrank.
func TestRouterRespectsAPortThatWillNotForward(t *testing.T) {
	tt := &fakeTunnels{forwardFail: true}
	r, opened, _ := router(t, tt)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := r.Open(ctx, "http://localhost:5173/"); err == nil {
		t.Fatal("a port that refuses to forward was opened")
	}
	if len(*opened) != 0 {
		t.Errorf("opened %v", *opened)
	}
	// Asking twice a second for the rest of the timeout would be noise.
	if len(tt.forwarded) != 1 {
		t.Errorf("asked to forward %d times, want exactly one attempt", len(tt.forwarded))
	}
}

// The original port being busy is the usual reason a tunnel was remapped in
// the first place. Tearing a working tunnel down to land on yet another
// arbitrary port helps nobody, and breaks any tab already open on it.
func TestRouterDoesNotRelocateOntoABusyPort(t *testing.T) {
	tt := &fakeTunnels{states: []tunnel.State{active(8976, 8977)}}
	r, opened, toasts := router(t, tt)
	r.canBind = func(int) bool { return false }

	if err := r.Open(context.Background(), "http://localhost:8976/callback"); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(tt.pinned) != 0 {
		t.Errorf("attempted to relocate onto a busy port: %v", tt.pinned)
	}
	if len(*opened) != 1 || (*opened)[0] != "http://localhost:8977/callback" {
		t.Fatalf("opened %v", *opened)
	}
	if !hasToast(*toasts, "may not match") {
		t.Errorf("toasts = %v, want the warning", *toasts)
	}
}

// The real probe has to agree with what the allocator will find.
func TestRouterFreeChecksTheRealPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	busy := ln.Addr().(*net.TCPAddr).Port

	r := &urlRouter{bind: "127.0.0.1"}
	if r.free(busy) {
		t.Errorf("port %d is listening but reported free", busy)
	}
	_ = ln.Close()
	if !r.free(busy) {
		t.Errorf("port %d was released but reported busy", busy)
	}
}
