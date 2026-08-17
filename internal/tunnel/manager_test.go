package tunnel

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jclement/autotun/internal/probe"
)

// echoServer is a stand-in for a remote dev server: it upper-cases whatever it
// is sent, so tests can prove bytes went through the tunnel in both directions.
type echoServer struct {
	ln net.Listener
}

func newEchoServer(t *testing.T) *echoServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("starting echo server: %v", err)
	}
	e := &echoServer{ln: ln}
	go e.serve()
	t.Cleanup(func() { ln.Close() })
	return e
}

func (e *echoServer) serve() {
	for {
		c, err := e.ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			buf := make([]byte, 4096)
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
}

func (e *echoServer) addr() string { return e.ln.Addr().String() }

// fixedDialer routes every dial to one address, standing in for the SSH
// transport, and records what it was asked for.
type fixedDialer struct {
	addr string

	mu     sync.Mutex
	dialed []string
	err    error
}

func (d *fixedDialer) Dial(network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.dialed = append(d.dialed, addr)
	err := d.err
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return net.Dial("tcp", d.addr)
}

func (d *fixedDialer) lastDial() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.dialed) == 0 {
		return ""
	}
	return d.dialed[len(d.dialed)-1]
}

func (d *fixedDialer) setErr(err error) {
	d.mu.Lock()
	d.err = err
	d.mu.Unlock()
}

// collector accumulates manager events.
type collector struct {
	mu     sync.Mutex
	events []Event
}

func (c *collector) add(e Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func (c *collector) kinds() []EventKind {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]EventKind, len(c.events))
	for i, e := range c.events {
		out[i] = e.Kind
	}
	return out
}

func (c *collector) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = nil
}

// snapshotOf builds a probe snapshot for the given ports.
func snapshotOf(ports ...int) probe.Snapshot {
	s := probe.Snapshot{}
	for i, p := range ports {
		s[p] = probe.Service{
			Port:  p,
			Binds: []probe.Bind{{Proto: "tcp", Addr: "127.0.0.1"}},
			PID:   1000 + i,
			Proc:  "node",
		}
	}
	return s
}

// newTestManager wires a manager to an echo server through a fixed dialer.
func newTestManager(t *testing.T, policy Policy) (*Manager, *fixedDialer, *collector) {
	t.Helper()
	echo := newEchoServer(t)
	d := &fixedDialer{addr: echo.addr()}
	c := &collector{}
	m := New(NewAllocator("127.0.0.1", false), d, Options{
		Policy:  policy,
		OnEvent: c.add,
	})
	t.Cleanup(m.Close)
	return m, d, c
}

// stateFor returns the manager's row for a remote port.
func stateFor(t *testing.T, m *Manager, port int) State {
	t.Helper()
	for _, s := range m.States() {
		if s.RemotePort == port {
			return s
		}
	}
	t.Fatalf("no state for remote port %d", port)
	return State{}
}

// roundTrip sends a line through a local tunnel port and returns the reply.
func roundTrip(t *testing.T, port int, msg string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 3*time.Second)
	if err != nil {
		t.Fatalf("dialing local tunnel port %d: %v", port, err)
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(conn, msg); err != nil {
		t.Fatalf("writing: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("reading: %v", err)
	}
	return string(buf)
}

func TestManagerForwardsANewService(t *testing.T) {
	m, dialer, events := newTestManager(t, DefaultPolicy())

	port := freePort(t)
	m.Sync(probe.Snapshot{}) // empty baseline, so the port counts as new
	m.Sync(snapshotOf(port))

	st := stateFor(t, m, port)
	if st.Status != StatusActive {
		t.Fatalf("status = %q, want active (skip: %q, err: %q)", st.Status, st.Skip, st.Err)
	}
	if st.LocalPort != port {
		t.Errorf("local port = %d, want the matching %d", st.LocalPort, port)
	}
	if st.Remapped {
		t.Error("a free port should not be marked remapped")
	}

	if got := roundTrip(t, st.LocalPort, "hello"); got != "HELLO" {
		t.Errorf("round trip = %q, want %q", got, "HELLO")
	}
	if want := fmt.Sprintf("127.0.0.1:%d", port); dialer.lastDial() != want {
		t.Errorf("dialed %q, want %q", dialer.lastDial(), want)
	}

	// Counters only settle once the connection has been torn down.
	waitUntil(t, func() bool {
		s := stateFor(t, m, port)
		return s.BytesIn == 5 && s.BytesOut == 5
	}, "byte counters to reach 5 each")

	st = stateFor(t, m, port)
	if st.TotalConns != 1 {
		t.Errorf("total connections = %d, want 1", st.TotalConns)
	}
	if kinds := events.kinds(); len(kinds) != 1 || kinds[0] != EventOpened {
		t.Errorf("events = %v, want one opened", kinds)
	}
}

func TestManagerSkipsPreexistingServices(t *testing.T) {
	m, _, _ := newTestManager(t, DefaultPolicy())

	existing, fresh := freePort(t), freePort(t)

	// The first scan defines the baseline.
	m.Sync(snapshotOf(existing))
	if st := stateFor(t, m, existing); st.Status != StatusSkipped || st.Skip != SkipPreexising {
		t.Fatalf("baseline service = %q/%q, want skipped/pre-existing", st.Status, st.Skip)
	}

	// Anything appearing later is forwarded.
	m.Sync(snapshotOf(existing, fresh))
	if st := stateFor(t, m, fresh); st.Status != StatusActive {
		t.Errorf("new service = %q, want active", st.Status)
	}
	if st := stateFor(t, m, existing); st.Status != StatusSkipped {
		t.Errorf("baseline service = %q, want still skipped", st.Status)
	}
}

func TestManagerExistingPolicyForwardsTheBaseline(t *testing.T) {
	p := DefaultPolicy()
	p.Existing = true
	m, _, _ := newTestManager(t, p)

	port := freePort(t)
	m.Sync(snapshotOf(port))

	if st := stateFor(t, m, port); st.Status != StatusActive {
		t.Errorf("status = %q, want active with --existing", st.Status)
	}
}

func TestManagerClosesTunnelsAfterTheGracePeriod(t *testing.T) {
	m, _, events := newTestManager(t, DefaultPolicy())

	a, b := freePort(t), freePort(t)
	m.Sync(snapshotOf(a, b)) // baseline
	m.Sync(snapshotOf(a, b, freePort(t)))
	events.reset()

	// One missed scan is tolerated, since a single dropped scan should not
	// tear down working tunnels.
	m.Sync(snapshotOf(a, b))
	if len(m.States()) != 3 {
		t.Errorf("after one miss there are %d rows, want 3", len(m.States()))
	}

	// The second consecutive miss removes it.
	m.Sync(snapshotOf(a, b))
	if len(m.States()) != 2 {
		t.Errorf("after two misses there are %d rows, want 2", len(m.States()))
	}
	if kinds := events.kinds(); len(kinds) != 1 || kinds[0] != EventClosed {
		t.Errorf("events = %v, want one closed", kinds)
	}
}

func TestManagerReleasesTheLocalPortOnTeardown(t *testing.T) {
	m, _, _ := newTestManager(t, DefaultPolicy())

	port := freePort(t)
	m.Sync(probe.Snapshot{}) // empty baseline
	m.Sync(snapshotOf(port)) // appears, gets forwarded
	local := stateFor(t, m, port).LocalPort

	m.Sync(probe.Snapshot{})
	m.Sync(probe.Snapshot{})

	waitUntil(t, func() bool {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", local))
		if err != nil {
			return false
		}
		ln.Close()
		return true
	}, "the local port to be released")
}

func TestManagerToggleAttachesAndDetaches(t *testing.T) {
	m, _, _ := newTestManager(t, DefaultPolicy())

	port := freePort(t)
	m.Sync(snapshotOf(port)) // baseline, so it starts skipped

	if !m.Toggle(port) {
		t.Fatal("Toggle should have attached the skipped service")
	}
	st := stateFor(t, m, port)
	if st.Status != StatusActive || !st.Manual {
		t.Fatalf("after attach: status %q manual %v, want active/true", st.Status, st.Manual)
	}
	if got := roundTrip(t, st.LocalPort, "up"); got != "UP" {
		t.Errorf("round trip = %q, want UP", got)
	}

	if m.Toggle(port) {
		t.Fatal("Toggle should have detached the active tunnel")
	}
	if st := stateFor(t, m, port); st.Status == StatusActive {
		t.Error("service should no longer be active")
	}

	// A manual override must survive later scans.
	m.Sync(snapshotOf(port))
	if st := stateFor(t, m, port); st.Status == StatusActive {
		t.Error("a manual detach should stick across scans")
	}
}

func TestManagerToggleUnknownPortIsANoop(t *testing.T) {
	m, _, _ := newTestManager(t, DefaultPolicy())
	if m.Toggle(9999) {
		t.Error("toggling an unknown port should report not-attached")
	}
}

func TestManagerSetPolicyReevaluatesEverything(t *testing.T) {
	m, _, _ := newTestManager(t, DefaultPolicy())

	port := freePort(t)
	m.Sync(snapshotOf(port)) // baseline: skipped
	if st := stateFor(t, m, port); st.Status == StatusActive {
		t.Fatal("expected the baseline service to start skipped")
	}

	p := m.Policy()
	p.Existing = true
	m.SetPolicy(p)

	if st := stateFor(t, m, port); st.Status != StatusActive {
		t.Errorf("status = %q, want active after enabling --existing", st.Status)
	}

	// Pausing tears the tunnel back down.
	p.Paused = true
	m.SetPolicy(p)
	if st := stateFor(t, m, port); st.Status == StatusActive {
		t.Error("pausing should close the tunnel")
	}
}

func TestManagerExcludeClosesAnOpenTunnel(t *testing.T) {
	m, _, _ := newTestManager(t, DefaultPolicy())

	port := freePort(t)
	m.Sync(probe.Snapshot{})
	m.Sync(snapshotOf(port))
	if st := stateFor(t, m, port); st.Status != StatusActive {
		t.Fatalf("status = %q, want active", st.Status)
	}

	p := m.Policy()
	p.Exclude, _ = ParsePortSet(fmt.Sprint(port))
	m.SetPolicy(p)

	st := stateFor(t, m, port)
	if st.Status == StatusActive {
		t.Error("excluding a port should close its tunnel")
	}
	if st.Skip != SkipExcluded {
		t.Errorf("skip reason = %q, want excluded", st.Skip)
	}
}

func TestManagerReportsAllocationFailure(t *testing.T) {
	echo := newEchoServer(t)
	c := &collector{}
	alloc := NewAllocator("127.0.0.1", true) // --same-port
	m := New(alloc, &fixedDialer{addr: echo.addr()}, Options{Policy: DefaultPolicy(), OnEvent: c.add})
	defer m.Close()

	busy := occupy(t)
	m.Sync(probe.Snapshot{})
	m.Sync(snapshotOf(busy))

	st := stateFor(t, m, busy)
	if st.Status != StatusError {
		t.Fatalf("status = %q, want error", st.Status)
	}
	if !strings.Contains(st.Err, "already in use") {
		t.Errorf("error = %q, want a port conflict message", st.Err)
	}
	if kinds := c.kinds(); len(kinds) != 1 || kinds[0] != EventFailed {
		t.Errorf("events = %v, want one failed", kinds)
	}

	// A repeated failure must not re-emit the same event on every scan.
	c.reset()
	m.Sync(snapshotOf(busy))
	if kinds := c.kinds(); len(kinds) != 0 {
		t.Errorf("events = %v, want the repeated failure to be suppressed", kinds)
	}
}

func TestManagerReconnectReusesTheSameLocalPort(t *testing.T) {
	m, dialer, _ := newTestManager(t, DefaultPolicy())

	port := freePort(t)
	m.Sync(probe.Snapshot{})
	m.Sync(snapshotOf(port))
	original := stateFor(t, m, port).LocalPort

	// The link drops.
	m.SetDialer(nil)
	if st := stateFor(t, m, port); st.Status == StatusActive {
		t.Error("tunnels should close when the transport goes away")
	}

	// And comes back.
	m.SetDialer(dialer)
	st := stateFor(t, m, port)
	if st.Status != StatusActive {
		t.Fatalf("status = %q, want active after reconnect", st.Status)
	}
	if st.LocalPort != original {
		t.Errorf("local port = %d after reconnect, want the original %d", st.LocalPort, original)
	}
	if got := roundTrip(t, st.LocalPort, "back"); got != "BACK" {
		t.Errorf("round trip = %q, want BACK", got)
	}
}

func TestManagerSurvivesADialFailure(t *testing.T) {
	m, dialer, _ := newTestManager(t, DefaultPolicy())

	port := freePort(t)
	m.Sync(probe.Snapshot{})
	m.Sync(snapshotOf(port))
	local := stateFor(t, m, port).LocalPort

	dialer.setErr(errors.New("channel refused"))

	// The local listener still accepts; the connection just fails fast.
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", local), 3*time.Second)
	if err != nil {
		t.Fatalf("local listener should stay up: %v", err)
	}
	_, _ = io.WriteString(conn, "x")
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadAll(conn); err != nil {
		t.Logf("read after failed dial: %v", err)
	}
	conn.Close()

	waitUntil(t, func() bool {
		return strings.Contains(stateFor(t, m, port).Err, "channel refused")
	}, "the dial error to surface on the row")

	// Recovery: once dialing works again, the same tunnel serves traffic.
	dialer.setErr(nil)
	if got := roundTrip(t, local, "ok"); got != "OK" {
		t.Errorf("round trip after recovery = %q, want OK", got)
	}
}

func TestManagerCounts(t *testing.T) {
	m, _, _ := newTestManager(t, DefaultPolicy())

	baseline := freePort(t)
	m.Sync(snapshotOf(baseline))
	fresh := freePort(t)
	m.Sync(snapshotOf(baseline, fresh))

	active, skipped, failed := m.Counts()
	if active != 1 || skipped != 1 || failed != 0 {
		t.Errorf("Counts() = %d/%d/%d, want 1 active, 1 skipped, 0 failed", active, skipped, failed)
	}
}

func TestManagerStatesAreSortedByRemotePort(t *testing.T) {
	m, _, _ := newTestManager(t, DefaultPolicy())
	m.Sync(probe.Snapshot{
		9000: {Port: 9000},
		3000: {Port: 3000},
		5000: {Port: 5000},
	})
	got := m.States()
	if len(got) != 3 || got[0].RemotePort != 3000 || got[1].RemotePort != 5000 || got[2].RemotePort != 9000 {
		t.Errorf("States() out of order: %+v", got)
	}
}

func TestManagerKeepsResolvedCommandLines(t *testing.T) {
	m, _, _ := newTestManager(t, DefaultPolicy())

	port := freePort(t)
	withCmd := probe.Snapshot{port: {Port: port, PID: 42, Proc: "node", Cmd: "node /app/server.js"}}
	m.Sync(withCmd)

	// A later scan that has not resolved the command line yet must not blank
	// out what we already know.
	m.Sync(probe.Snapshot{port: {Port: port, PID: 42, Proc: "node"}})
	if got := stateFor(t, m, port).Cmd; got != "node /app/server.js" {
		t.Errorf("command = %q, want the previously resolved one", got)
	}
}

func TestStateURL(t *testing.T) {
	tests := []struct {
		name string
		st   State
		want string
	}{
		{"active loopback", State{Status: StatusActive, LocalAddr: "127.0.0.1", LocalPort: 3000}, "http://127.0.0.1:3000"},
		{"wildcard becomes localhost", State{Status: StatusActive, LocalAddr: "0.0.0.0", LocalPort: 3000}, "http://localhost:3000"},
		{"skipped has no URL", State{Status: StatusSkipped, LocalPort: 3000}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.st.URL(); got != tt.want {
				t.Errorf("URL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEventString(t *testing.T) {
	st := State{RemotePort: 3000, LocalPort: 3000, LocalAddr: "127.0.0.1", Cmd: "node"}
	tests := []struct {
		e    Event
		want string
	}{
		{Event{Kind: EventOpened, State: st}, "opened  127.0.0.1:3000 → remote 3000  (node)"},
		{Event{Kind: EventOpened, State: State{RemotePort: 3000, LocalPort: 3001, LocalAddr: "127.0.0.1", Remapped: true, Cmd: "node"}},
			"opened  127.0.0.1:3001 ≠ remote 3000  (node)"},
		{Event{Kind: EventClosed, State: st, Msg: "gone"}, "closed  127.0.0.1:3000    remote 3000  (gone)"},
		{Event{Kind: EventFailed, State: st, Msg: "busy"}, "failed  remote 3000  (busy)"},
		{Event{Kind: EventSkipped, State: st, Msg: "excluded"}, "skipped remote 3000  (excluded)"},
	}
	for _, tt := range tests {
		if got := tt.e.String(); got != tt.want {
			t.Errorf("String() = %q, want %q", got, tt.want)
		}
	}
}

// waitUntil polls cond, failing the test if it never becomes true.
func waitUntil(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
