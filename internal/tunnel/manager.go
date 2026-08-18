package tunnel

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jclement/autotun/internal/config"
	"github.com/jclement/autotun/internal/probe"
)

// Status is the lifecycle state of a discovered remote service.
type Status string

const (
	// StatusActive means a local listener is open and forwarding.
	StatusActive Status = "active"
	// StatusSkipped means the service is known but policy says don't forward.
	StatusSkipped Status = "skipped"
	// StatusError means forwarding was attempted and failed, usually a local
	// port conflict under --same-port.
	StatusError Status = "error"
	// StatusOffline means the tunnel existed but the SSH link is down.
	StatusOffline Status = "offline"
)

// State is an immutable snapshot of one row for the UI. Values are copied out
// under the manager lock so renderers never touch live tunnel state.
type State struct {
	RemotePort int
	LocalPort  int
	LocalAddr  string
	Remapped   bool

	PID   int
	Proc  string
	Cmd   string
	Binds string

	Status Status
	Skip   Skip
	Err    string
	Scheme Scheme
	// SchemePinned reports that the user chose the scheme, rather than it
	// being detected. Pinned choices are remembered between runs.
	SchemePinned bool
	// Label is the user's remembered name for this host and port.
	Label string

	// Mode is the user's standing decision for this port.
	Mode        config.Mode
	Preexisting bool
	// PinnedLocal is a local port the user chose, or zero.
	PinnedLocal int

	FirstSeen time.Time
	Created   time.Time
	LastByte  time.Time

	BytesIn     uint64
	BytesOut    uint64
	ActiveConns int
	TotalConns  uint64
}

// URL returns the address to point a browser at, or "" when not forwarding.
func (s State) URL() string {
	if s.Status != StatusActive || s.Scheme == SchemeUnknown {
		return ""
	}
	return s.Scheme.URLScheme() + "://" + s.Endpoint()
}

// Endpoint returns the local host:port for an active tunnel. It is useful for
// raw TCP services where inventing an HTTP URL would be misleading.
func (s State) Endpoint() string {
	if s.Status != StatusActive {
		return ""
	}
	host := s.LocalAddr
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return net.JoinHostPort(host, strconv.Itoa(s.LocalPort))
}

// EventKind classifies a change the manager made.
type EventKind string

const (
	EventOpened  EventKind = "opened"
	EventClosed  EventKind = "closed"
	EventFailed  EventKind = "failed"
	EventSkipped EventKind = "skipped"
)

// Event describes one change, for the log and NDJSON renderers.
type Event struct {
	Kind  EventKind
	At    time.Time
	State State
	Msg   string
}

func (e Event) String() string {
	s := e.State
	switch e.Kind {
	case EventOpened:
		arrow := "→"
		if s.Remapped {
			arrow = "≠"
		}
		return fmt.Sprintf("opened  %s:%d %s remote %d  (%s)", s.LocalAddr, s.LocalPort, arrow, s.RemotePort, s.Cmd)
	case EventClosed:
		return fmt.Sprintf("closed  %s:%d    remote %d  (%s)", s.LocalAddr, s.LocalPort, s.RemotePort, e.Msg)
	case EventFailed:
		return fmt.Sprintf("failed  remote %d  (%s)", s.RemotePort, e.Msg)
	default:
		return fmt.Sprintf("skipped remote %d  (%s)", s.RemotePort, e.Msg)
	}
}

// entry is the manager's private record for one remote port.
type entry struct {
	svc         probe.Service
	fwd         *forwarder
	skip        Skip
	err         string
	mode        config.Mode // auto follows policy; on and off are user decisions
	pinnedLocal int         // a local port the user chose, or zero
	label       string      // a human-friendly name remembered for this host/port
	preexisting bool
	// pausedKeep means this automatic tunnel was already active when forwarding
	// was paused. It stays up (and returns after a reconnect) while new automatic
	// tunnels remain suspended.
	pausedKeep bool
	firstSeen  time.Time
	missing    int

	scheme Scheme
	pinned bool // the user chose the scheme; never overwrite it by observation
}

// Manager keeps local listeners in sync with the set of services discovered on
// the remote host.
//
// It is safe for concurrent use: Sync runs on the poller goroutine while the UI
// calls States and the mutators.
type Manager struct {
	alloc  *Allocator
	grace  int // consecutive missed scans tolerated before teardown
	now    func() time.Time
	onEvt  func(Event)
	mu     sync.Mutex
	dialer Dialer

	policy   Policy
	entries  map[int]*entry
	baseline map[int]bool
	haveBase bool

	host     string
	settings Settings
}

// Settings remembers per-port decisions across runs. *config.Store satisfies
// it.
type Settings interface {
	Port(host string, port int) config.Port
	SetPort(host string, port int, p config.Port)
}

// Options configures a Manager.
type Options struct {
	Policy Policy
	// Host names the remote for scheme memory lookups.
	Host string
	// Settings, if set, persists the user's per-port decisions.
	Settings Settings
	// Grace is how many consecutive scans a service may be absent before its
	// tunnel is torn down. Two absorbs a single dropped scan without leaving
	// dead listeners around.
	Grace int
	// OnEvent, if set, receives every change. It is called without the
	// manager lock held.
	OnEvent func(Event)
	// Now is injectable for deterministic tests.
	Now func() time.Time
}

// New returns a Manager that allocates through alloc and forwards over dialer.
// dialer may be nil initially and supplied later with SetDialer.
func New(alloc *Allocator, dialer Dialer, opts Options) *Manager {
	if opts.Grace <= 0 {
		opts.Grace = 2
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Policy.MaxPort == 0 {
		opts.Policy.MaxPort = 65535
	}
	return &Manager{
		alloc:    alloc,
		dialer:   dialer,
		grace:    opts.Grace,
		now:      opts.Now,
		onEvt:    opts.OnEvent,
		policy:   opts.Policy,
		entries:  map[int]*entry{},
		baseline: map[int]bool{},
		host:     opts.Host,
		settings: opts.Settings,
	}
}

// Sync reconciles the live tunnels with a freshly observed snapshot. The first
// call also establishes the pre-existing baseline.
func (m *Manager) Sync(snap probe.Snapshot) {
	var events []Event

	m.mu.Lock()
	now := m.now()
	if !m.haveBase {
		for port := range snap {
			m.baseline[port] = true
		}
		m.haveBase = true
	}

	for port, svc := range snap {
		e, ok := m.entries[port]
		if !ok {
			e = &entry{
				svc:         svc,
				preexisting: m.baseline[port],
				firstSeen:   now,
			}
			// Restore whatever the user decided about this port last time.
			e.mode = config.ModeAuto
			if m.settings != nil {
				saved := m.settings.Port(m.host, port)
				e.scheme = ParseScheme(saved.Scheme)
				e.pinned = e.scheme != SchemeUnknown
				e.mode = config.ParseMode(saved.Mode)
				e.pinnedLocal = saved.Local
				e.label = saved.Label
			}
			m.entries[port] = e
		}
		e.missing = 0
		// Keep an already-resolved command line if this scan lost it.
		if svc.Cmd == "" {
			svc.Cmd = e.svc.Cmd
		}
		e.svc = svc
		events = append(events, m.applyLocked(e, now)...)
	}

	for port, e := range m.entries {
		if _, ok := snap[port]; ok {
			continue
		}
		e.missing++
		if e.missing < m.grace {
			continue
		}
		if e.fwd != nil {
			events = append(events, m.closeLocked(e, "remote service went away"))
		}
		delete(m.entries, port)
	}
	m.mu.Unlock()

	m.emit(events)
}

// applyLocked opens or closes the tunnel for e to match current policy.
// Caller holds m.mu.
func (m *Manager) applyLocked(e *entry, now time.Time) []Event {
	want := m.wantLocked(e)

	if !want.forward {
		e.skip = want.skip
		if e.fwd != nil {
			return []Event{m.closeLocked(e, string(want.skip))}
		}
		return nil
	}

	e.skip = SkipNone
	if e.fwd != nil {
		return nil
	}
	if m.dialer == nil {
		return nil // link is down; Sync will retry once reconnected
	}

	ln, remapped, err := m.alloc.Allocate(e.svc.Port, e.pinnedLocal)
	if err != nil {
		if e.err == err.Error() {
			return nil // already reported; don't spam the log each scan
		}
		e.err = err.Error()
		return []Event{{Kind: EventFailed, At: now, State: m.stateLocked(e), Msg: e.err}}
	}
	e.err = ""
	port := e.svc.Port
	// What a service turns out to be is learned from the replies it sends
	// through the tunnel, never by probing it.
	e.fwd = newForwarder(ln, m.dialer, e.svc.DialAddr(), remapped, now, func(scheme Scheme) {
		m.observeScheme(port, scheme)
	})
	return []Event{{Kind: EventOpened, At: now, State: m.stateLocked(e)}}
}

// observeScheme records what a service revealed itself to be. A scheme the
// user pinned is never overwritten.
func (m *Manager) observeScheme(port int, scheme Scheme) {
	if scheme == SchemeUnknown {
		return
	}
	m.mu.Lock()
	e, ok := m.entries[port]
	changed := ok && !e.pinned && e.scheme != scheme
	if changed {
		e.scheme = scheme
	}
	m.mu.Unlock()
}

// CycleScheme steps a port's scheme through unknown → http → https and
// remembers the choice. Returns the new scheme.
func (m *Manager) CycleScheme(remotePort int) Scheme {
	m.mu.Lock()
	e, ok := m.entries[remotePort]
	if !ok {
		m.mu.Unlock()
		return SchemeUnknown
	}
	e.scheme = e.scheme.Next()
	// Cycling back to unknown releases the pin, so detection may run again on
	// a later reconnect.
	e.pinned = e.scheme != SchemeUnknown
	scheme := e.scheme
	m.mu.Unlock()

	m.persist(remotePort)
	return scheme
}

type want struct {
	forward bool
	skip    Skip
}

// wantLocked applies the user's standing decision for the port if there is
// one, otherwise the policy.
func (m *Manager) wantLocked(e *entry) want {
	switch e.mode {
	case config.ModeOn:
		return want{forward: true}
	case config.ModeOff:
		return want{skip: SkipOff}
	}
	if m.policy.Paused && e.pausedKeep {
		return want{forward: true}
	}
	skip := m.policy.Eval(e.svc, e.preexisting)
	return want{forward: skip == SkipNone, skip: skip}
}

// closeLocked tears down e's forwarder. Caller holds m.mu.
func (m *Manager) closeLocked(e *entry, reason string) Event {
	fwd := e.fwd
	e.fwd = nil
	st := m.stateLocked(e)
	st.LocalPort = fwd.local
	st.Status = StatusSkipped
	// Release the local port synchronously so a reconnect can immediately
	// re-bind it, but drain in-flight connections in the background rather
	// than blocking a scan on a slow peer.
	fwd.stop()
	go fwd.Close()
	return Event{Kind: EventClosed, At: m.now(), State: st, Msg: reason}
}

// stateLocked snapshots e for display. Caller holds m.mu.
func (m *Manager) stateLocked(e *entry) State {
	st := State{
		RemotePort:   e.svc.Port,
		LocalAddr:    m.alloc.Bind(),
		PID:          e.svc.PID,
		Proc:         e.svc.Proc,
		Cmd:          e.svc.Command(),
		Binds:        e.svc.BindSummary(),
		Skip:         e.skip,
		Err:          e.err,
		Mode:         e.mode,
		Preexisting:  e.preexisting,
		PinnedLocal:  e.pinnedLocal,
		Label:        e.label,
		FirstSeen:    e.firstSeen,
		Scheme:       e.scheme,
		SchemePinned: e.pinned,
	}
	switch {
	case e.fwd != nil:
		f := e.fwd
		st.Status = StatusActive
		st.LocalPort = f.local
		st.Remapped = f.remapped
		st.Created = f.created
		st.BytesIn = f.in.Load()
		st.BytesOut = f.out.Load()
		st.ActiveConns = int(f.active.Load())
		st.TotalConns = f.total.Load()
		if ns := f.last.Load(); ns != 0 {
			st.LastByte = time.Unix(0, ns)
		}
		if msg := f.err(); msg != "" {
			st.Err = msg
		}
	case e.err != "":
		st.Status = StatusError
	case m.dialer == nil && m.wantLocked(e).forward:
		// The link is down but the port is still ours: the local port stays
		// reserved and comes back on the same number.
		st.Status = StatusOffline
	default:
		st.Status = StatusSkipped
	}
	return st
}

// hiddenLocked reports whether an entry is filtered out of the table entirely.
// A standing user decision or a live tunnel always keeps a row visible.
func (m *Manager) hiddenLocked(e *entry) bool {
	return e.fwd == nil && e.mode == config.ModeAuto && e.skip.Filtered()
}

// States returns every service worth showing, ordered by remote port.
func (m *Manager) States() []State {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]State, 0, len(m.entries))
	for _, e := range m.entries {
		if m.hiddenLocked(e) {
			continue
		}
		out = append(out, m.stateLocked(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RemotePort < out[j].RemotePort })
	return out
}

// Policy returns the active policy.
func (m *Manager) Policy() Policy {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.policy
}

// SetPolicy replaces the policy and reconciles every known service against it.
func (m *Manager) SetPolicy(p Policy) {
	m.mu.Lock()
	wasPaused := m.policy.Paused
	if !wasPaused && p.Paused {
		for _, e := range m.entries {
			e.pausedKeep = e.mode == config.ModeAuto && e.fwd != nil
		}
	} else if wasPaused && !p.Paused {
		for _, e := range m.entries {
			e.pausedKeep = false
		}
	}
	m.policy = p
	now := m.now()
	var events []Event
	for _, e := range m.sortedEntriesLocked() {
		events = append(events, m.applyLocked(e, now)...)
	}
	m.mu.Unlock()
	m.emit(events)
}

// CycleMode steps a port through auto → on → off and remembers the choice.
func (m *Manager) CycleMode(remotePort int) config.Mode {
	m.mu.Lock()
	e, ok := m.entries[remotePort]
	if !ok {
		m.mu.Unlock()
		return config.ModeAuto
	}
	e.mode = e.mode.Next()
	if e.mode != config.ModeAuto {
		e.pausedKeep = false
	}
	if e.mode != config.ModeOff {
		e.err = "" // choosing to forward is an explicit retry
	}
	events := m.applyLocked(e, m.now())
	mode := e.mode
	m.mu.Unlock()

	m.persist(remotePort)
	m.emit(events)
	return mode
}

// SetScheme sets a port's protocol directly and remembers it. Unknown clears
// the pin so passive detection may classify future traffic again.
func (m *Manager) SetScheme(remotePort int, scheme Scheme) Scheme {
	scheme = ParseScheme(string(scheme))
	m.mu.Lock()
	e, ok := m.entries[remotePort]
	if !ok {
		m.mu.Unlock()
		return SchemeUnknown
	}
	e.scheme = scheme
	e.pinned = scheme != SchemeUnknown
	m.mu.Unlock()
	m.persist(remotePort)
	return scheme
}

// SetLabel gives a remote port a human-friendly name and remembers it. An
// empty label restores the process command as the row's primary identity.
func (m *Manager) SetLabel(remotePort int, label string) error {
	label = strings.Join(strings.Fields(label), " ")
	m.mu.Lock()
	e, ok := m.entries[remotePort]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("remote port %d is not listed", remotePort)
	}
	e.label = label
	m.mu.Unlock()
	m.persist(remotePort)
	return nil
}

// SetLocalPort pins the local port a service is forwarded to. Zero restores
// the default of mirroring the remote port. The tunnel is reopened so the
// change takes effect immediately.
func (m *Manager) SetLocalPort(remotePort, local int) error {
	if local < 0 || local > 65535 {
		return fmt.Errorf("local port %d is out of range", local)
	}

	m.mu.Lock()
	e, ok := m.entries[remotePort]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("remote port %d is not listed", remotePort)
	}
	e.pinnedLocal = local
	// Drop the remembered assignment so the new preference is not overruled
	// by where this service happened to land last time.
	m.alloc.Forget(remotePort)

	var events []Event
	if e.fwd != nil {
		events = append(events, m.closeLocked(e, "relocating"))
	}
	e.err = ""
	events = append(events, m.applyLocked(e, m.now())...)
	failed := e.err
	m.mu.Unlock()

	m.persist(remotePort)
	m.emit(events)
	if failed != "" {
		return errors.New(failed)
	}
	return nil
}

// persist writes a port's current decisions to the settings store.
func (m *Manager) persist(remotePort int) {
	if m.settings == nil {
		return
	}
	m.mu.Lock()
	e, ok := m.entries[remotePort]
	if !ok {
		m.mu.Unlock()
		return
	}
	saved := config.Port{Label: e.label, Mode: string(e.mode), Local: e.pinnedLocal}
	if e.pinned {
		saved.Scheme = string(e.scheme)
	}
	host := m.host
	settings := m.settings
	m.mu.Unlock()

	settings.SetPort(host, remotePort, saved)
}

// SetDialer swaps the transport, used when the SSH link is re-established.
// Passing nil marks the link down and closes every forwarder; the local port
// assignments are remembered so reconnecting restores them.
func (m *Manager) SetDialer(d Dialer) {
	m.mu.Lock()
	m.dialer = d
	var events []Event
	if d == nil {
		for _, e := range m.sortedEntriesLocked() {
			if e.fwd != nil {
				events = append(events, m.closeLocked(e, "disconnected"))
			}
			e.missing = 0
		}
	} else {
		now := m.now()
		for _, e := range m.sortedEntriesLocked() {
			events = append(events, m.applyLocked(e, now)...)
		}
	}
	m.mu.Unlock()
	m.emit(events)
}

// Close tears down every tunnel.
func (m *Manager) Close() {
	m.mu.Lock()
	fwds := make([]*forwarder, 0, len(m.entries))
	for _, e := range m.entries {
		if e.fwd != nil {
			fwds = append(fwds, e.fwd)
			e.fwd = nil
		}
	}
	m.mu.Unlock()
	for _, f := range fwds {
		f.Close()
	}
}

// Counts summarizes the current tunnel set.
func (m *Manager) Counts() (active, skipped, failed int) {
	for _, s := range m.States() {
		switch s.Status {
		case StatusActive:
			active++
		case StatusError:
			failed++
		default:
			skipped++
		}
	}
	return
}

func (m *Manager) sortedEntriesLocked() []*entry {
	out := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].svc.Port < out[j].svc.Port })
	return out
}

func (m *Manager) emit(events []Event) {
	if m.onEvt == nil {
		return
	}
	for _, e := range events {
		m.onEvt(e)
	}
}
