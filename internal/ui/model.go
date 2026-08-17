package ui

import (
	"math/rand"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jclement/autotun/internal/tunnel"
)

// Controller is the slice of the tunnel manager the UI drives. Keeping it an
// interface means the whole view layer is testable with a stub.
type Controller interface {
	States() []tunnel.State
	Toggle(remotePort int) bool
	CycleScheme(remotePort int) tunnel.Scheme
	Policy() tunnel.Policy
	SetPolicy(tunnel.Policy)
}

// ConnState is the SSH link's status, shown in the header.
type ConnState string

const (
	Connecting   ConnState = "connecting"
	Connected    ConnState = "connected"
	Reconnecting ConnState = "reconnecting"
	Disconnected ConnState = "disconnected"
)

// StatusMsg updates the connection indicator. The app sends these as the SSH
// link comes and goes.
type StatusMsg struct {
	State   ConnState
	Mode    string // remote discovery mode, e.g. "ss"
	Detail  string // error text or reconnect reason
	Attempt int
}

// ToastMsg shows a transient message in the footer.
type ToastMsg struct {
	Text string
	Bad  bool
}

// FatalMsg terminates the UI with an error.
type FatalMsg struct{ Err error }

type refreshMsg time.Time
type dissolveMsg time.Time
type toastExpiredMsg int

const (
	refreshInterval  = 400 * time.Millisecond
	dissolveInterval = 33 * time.Millisecond
	toastDuration    = 3 * time.Second
)

// Options configures the model.
type Options struct {
	// Host is the label shown in the header.
	Host string
	// Version is shown in the help box.
	Version string
	// Dissolve enables the exit animation.
	Dissolve bool
	// Theme overrides the default styling.
	Theme *Theme
	// Now is injectable for deterministic tests.
	Now func() time.Time
	// Rand seeds the dissolve animation; nil uses a time-seeded source.
	Rand *rand.Rand
	// OpenURL is called for the `o` key. Nil uses the platform opener.
	OpenURL func(string) error
}

// Model is the bubbletea model for autotun's table UI.
type Model struct {
	ctrl Controller
	opts Options
	th   Theme
	now  func() time.Time
	rng  *rand.Rand

	width, height int
	started       time.Time

	rows    []tunnel.State
	cursor  int
	offset  int
	sortKey SortKey
	reverse bool

	filtering bool
	filter    textInput
	query     string

	showHelp   bool
	showDetail bool
	confirming bool

	status   StatusMsg
	toast    ToastMsg
	toastID  int
	hasToast bool

	lastClickPort int
	lastClickAt   time.Time

	dissolving *dissolve
	// pendingOSC is an escape sequence emitted with the next frame. Writing
	// it as part of the frame keeps it from racing the renderer.
	pendingOSC string

	fatal error
	quit  bool
}

// New builds a Model.
func New(ctrl Controller, opts Options) *Model {
	th := DefaultTheme()
	if opts.Theme != nil {
		th = *opts.Theme
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	rng := opts.Rand
	if rng == nil {
		rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	if opts.OpenURL == nil {
		opts.OpenURL = OpenURL
	}
	return &Model{
		ctrl:    ctrl,
		opts:    opts,
		th:      th,
		now:     now,
		rng:     rng,
		started: now(),
		width:   80,
		height:  24,
		status:  StatusMsg{State: Connecting},
		rows:    ctrl.States(),
	}
}

// Init starts the refresh ticker.
func (m *Model) Init() tea.Cmd {
	return tick(refreshInterval, func(t time.Time) tea.Msg { return refreshMsg(t) })
}

func tick(d time.Duration, fn func(time.Time) tea.Msg) tea.Cmd {
	return tea.Tick(d, fn)
}

// Err returns the error the UI exited with, if any.
func (m *Model) Err() error { return m.fatal }

// Update handles one message.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Any message clears a sequence queued for the previous frame.
	m.pendingOSC = ""

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.clampCursor()
		return m, nil

	case FatalMsg:
		m.fatal = msg.Err
		return m, tea.Quit

	case StatusMsg:
		m.status = msg
		return m, nil

	case ToastMsg:
		return m, m.showToast(msg)

	case toastExpiredMsg:
		if int(msg) == m.toastID {
			m.hasToast = false
		}
		return m, nil

	case refreshMsg:
		if m.dissolving == nil {
			m.reload()
		}
		return m, tick(refreshInterval, func(t time.Time) tea.Msg { return refreshMsg(t) })

	case dissolveMsg:
		if m.dissolving == nil {
			return m, nil
		}
		m.dissolving.Advance()
		if m.dissolving.Done() {
			m.quit = true
			return m, tea.Quit
		}
		return m, tick(dissolveInterval, func(t time.Time) tea.Msg { return dissolveMsg(t) })

	case tea.MouseMsg:
		return m, m.handleMouse(msg)

	case tea.KeyMsg:
		return m, m.handleKey(msg)
	}
	return m, nil
}

// doubleClickWindow is how close two clicks must be to count as a double click.
const doubleClickWindow = 450 * time.Millisecond

// handleMouse maps clicks onto rows: one click selects, a double click opens
// the tunnel in a browser, and the wheel scrolls.
func (m *Model) handleMouse(msg tea.MouseMsg) tea.Cmd {
	if m.dissolving != nil || m.confirming || m.filtering {
		return nil
	}

	switch msg.Button {
	case tea.MouseButtonWheelUp:
		m.cursor--
		m.clampCursor()
		return nil
	case tea.MouseButtonWheelDown:
		m.cursor++
		m.clampCursor()
		return nil
	}

	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return nil
	}

	idx := m.rowAt(msg.Y)
	if idx < 0 {
		return nil
	}
	m.cursor = idx
	m.clampCursor()

	now := m.now()
	port := m.selectedPort()
	isDouble := port == m.lastClickPort && now.Sub(m.lastClickAt) < doubleClickWindow
	m.lastClickPort, m.lastClickAt = port, now

	if !isDouble {
		return nil
	}
	m.lastClickPort = 0 // a third click starts a new pair
	if st, ok := m.selected(); ok && st.Scheme == tunnel.SchemeUnknown {
		return m.showToast(ToastMsg{Text: "unknown protocol — press t to set http/https", Bad: true})
	}
	return m.openSelected()
}

// rowAt maps a terminal row to a table index, or -1 if it is not a data row.
func (m *Model) rowAt(y int) int {
	idx := y - headerLines + m.offset
	if y < headerLines || idx >= len(m.rows) || idx-m.offset >= m.tableHeight() {
		return -1
	}
	return idx
}

// reload pulls fresh state, preserving the selected port across reorderings.
func (m *Model) reload() {
	selected := m.selectedPort()
	rows := m.ctrl.States()
	rows = filterStates(rows, m.query)
	sortStates(rows, m.sortKey, m.reverse)
	m.rows = rows

	if selected > 0 {
		for i, r := range rows {
			if r.RemotePort == selected {
				m.cursor = i
				break
			}
		}
	}
	m.clampCursor()
}

func (m *Model) selectedPort() int {
	if m.cursor >= 0 && m.cursor < len(m.rows) {
		return m.rows[m.cursor].RemotePort
	}
	return 0
}

func (m *Model) selected() (tunnel.State, bool) {
	if m.cursor >= 0 && m.cursor < len(m.rows) {
		return m.rows[m.cursor], true
	}
	return tunnel.State{}, false
}

func (m *Model) clampCursor() {
	if m.cursor >= len(m.rows) {
		m.cursor = len(m.rows) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	h := m.tableHeight()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if h > 0 && m.cursor >= m.offset+h {
		m.offset = m.cursor - h + 1
	}
	if max := len(m.rows) - h; m.offset > max {
		m.offset = max
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

func (m *Model) showToast(t ToastMsg) tea.Cmd {
	m.toast = t
	m.hasToast = true
	m.toastID++
	id := m.toastID
	return tea.Tick(toastDuration, func(time.Time) tea.Msg { return toastExpiredMsg(id) })
}

// handleKey routes a key press through the modal layers, innermost first.
func (m *Model) handleKey(msg tea.KeyMsg) tea.Cmd {
	if m.dissolving != nil {
		// Once the screen is falling apart, any key skips to the end.
		m.quit = true
		return tea.Quit
	}
	if m.confirming {
		return m.handleConfirmKey(msg)
	}
	if m.filtering {
		return m.handleFilterKey(msg)
	}
	if m.showHelp {
		switch msg.String() {
		case "?", "esc", "q", "enter":
			m.showHelp = false
			return nil
		}
	}
	if m.showDetail {
		switch msg.String() {
		case "esc", "enter", "d", "q":
			m.showDetail = false
			return nil
		}
	}
	return m.handleTableKey(msg)
}

func (m *Model) handleConfirmKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "y", "Y", "enter":
		m.confirming = false
		return m.beginQuit()
	case "n", "N", "esc", "q", "ctrl+c":
		m.confirming = false
		return nil
	}
	return nil
}

func (m *Model) handleFilterKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.filtering = false
		m.filter.Reset()
		m.query = ""
		m.reload()
		return nil
	case "enter":
		m.filtering = false
		return nil
	case "ctrl+c":
		m.filtering = false
		m.filter.Reset()
		m.query = ""
		m.reload()
		return nil
	}
	if m.filter.Update(msg) {
		m.query = m.filter.Value()
		m.cursor = 0
		m.reload()
	}
	return nil
}

func (m *Model) handleTableKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "ctrl+c":
		// Ctrl-C is the impatient exit: no confirmation, no animation.
		m.quit = true
		return tea.Quit

	case "q", "esc":
		m.confirming = true
		return nil

	case "up", "k":
		m.cursor--
		m.clampCursor()
	case "down", "j":
		m.cursor++
		m.clampCursor()
	case "pgup", "ctrl+u":
		m.cursor -= m.tableHeight()
		m.clampCursor()
	case "pgdown", "ctrl+d":
		m.cursor += m.tableHeight()
		m.clampCursor()
	case "home", "g":
		m.cursor = 0
		m.clampCursor()
	case "end", "G":
		m.cursor = len(m.rows) - 1
		m.clampCursor()

	case "s":
		m.sortKey = m.sortKey.Next()
		m.reload()
		return m.showToast(ToastMsg{Text: "sort: " + m.sortKey.String()})
	case "r":
		m.reverse = !m.reverse
		m.reload()

	case "/":
		m.filtering = true
		m.filter.SetValue(m.query)
		return nil

	case "?":
		m.showHelp = !m.showHelp
	case "enter", "d":
		if _, ok := m.selected(); ok {
			m.showDetail = !m.showDetail
		}

	case "a":
		return m.toggleSelected()
	case "t":
		return m.cycleScheme()
	case " ":
		// Space opens only when we know the row speaks HTTP(S), so it is safe
		// to lean on without checking what a port is first.
		if st, ok := m.selected(); ok && st.Scheme == tunnel.SchemeUnknown {
			return m.showToast(ToastMsg{Text: "unknown protocol — press t to set http/https", Bad: true})
		}
		return m.openSelected()
	case "o":
		return m.openSelected()
	case "y":
		return m.copySelected()
	case "p":
		return m.togglePause()
	case "e":
		return m.toggleExisting()
	}
	return nil
}

func (m *Model) toggleSelected() tea.Cmd {
	st, ok := m.selected()
	if !ok {
		return nil
	}
	attached := m.ctrl.Toggle(st.RemotePort)
	m.reload()
	if attached {
		return m.showToast(ToastMsg{Text: "attached remote " + itoa(st.RemotePort)})
	}
	return m.showToast(ToastMsg{Text: "detached remote " + itoa(st.RemotePort)})
}

// cycleScheme steps the selected row through unknown → http → https.
func (m *Model) cycleScheme() tea.Cmd {
	st, ok := m.selected()
	if !ok {
		return nil
	}
	scheme := m.ctrl.CycleScheme(st.RemotePort)
	m.reload()
	if scheme == tunnel.SchemeUnknown {
		return m.showToast(ToastMsg{Text: "remote " + itoa(st.RemotePort) + ": protocol unset"})
	}
	return m.showToast(ToastMsg{Text: "remote " + itoa(st.RemotePort) + " is " + string(scheme) + " (remembered)"})
}

func (m *Model) openSelected() tea.Cmd {
	st, ok := m.selected()
	if !ok {
		return nil
	}
	url := st.URL()
	if url == "" {
		return m.showToast(ToastMsg{Text: "no tunnel to open", Bad: true})
	}
	if err := m.opts.OpenURL(url); err != nil {
		return m.showToast(ToastMsg{Text: "open failed: " + err.Error(), Bad: true})
	}
	return m.showToast(ToastMsg{Text: "opened " + url})
}

func (m *Model) copySelected() tea.Cmd {
	st, ok := m.selected()
	if !ok {
		return nil
	}
	url := st.URL()
	if url == "" {
		return m.showToast(ToastMsg{Text: "no tunnel to copy", Bad: true})
	}
	m.pendingOSC = OSC52(url)
	return m.showToast(ToastMsg{Text: "copied " + url})
}

func (m *Model) togglePause() tea.Cmd {
	p := m.ctrl.Policy()
	p.Paused = !p.Paused
	m.ctrl.SetPolicy(p)
	m.reload()
	if p.Paused {
		return m.showToast(ToastMsg{Text: "paused: new ports will not be forwarded"})
	}
	return m.showToast(ToastMsg{Text: "resumed"})
}

func (m *Model) toggleExisting() tea.Cmd {
	p := m.ctrl.Policy()
	p.Existing = !p.Existing
	m.ctrl.SetPolicy(p)
	m.reload()
	if p.Existing {
		return m.showToast(ToastMsg{Text: "forwarding pre-existing ports too"})
	}
	return m.showToast(ToastMsg{Text: "ignoring pre-existing ports"})
}

// beginQuit starts the dissolve, or quits immediately when it is disabled.
func (m *Model) beginQuit() tea.Cmd {
	if !m.opts.Dissolve || m.width <= 0 || m.height <= 0 {
		m.quit = true
		return tea.Quit
	}
	m.dissolving = newDissolve(m.baseView(), m.width, m.height, m.th, m.rng)
	return tick(dissolveInterval, func(t time.Time) tea.Msg { return dissolveMsg(t) })
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
