package ui

import (
	"math/rand"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jclement/autotun/internal/config"
	"github.com/jclement/autotun/internal/tunnel"
)

// Controller is the slice of the tunnel manager the UI drives. Keeping it an
// interface means the whole view layer is testable with a stub.
type Controller interface {
	States() []tunnel.State
	CycleMode(remotePort int) config.Mode
	CycleScheme(remotePort int) tunnel.Scheme
	SetLocalPort(remotePort, local int) error
	Policy() tunnel.Policy
	SetPolicy(tunnel.Policy)
	// ViewPrefs and SetViewPrefs carry the per-host presentation settings,
	// which the controller persists.
	ViewPrefs() config.ViewPrefs
	SetViewPrefs(config.ViewPrefs)
}

// ConnState is the SSH link's status, shown in the header.
type ConnState string

const (
	Connecting ConnState = "connecting"
	// Probing is the gap between a working SSH connection and the first scan,
	// while the remote picks a discovery tool and starts reporting.
	Probing      ConnState = "probing"
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
	// NextRetry is when the next attempt is due, so the wait can be counted
	// down rather than left to look like a hang.
	NextRetry time.Time
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
	// Retry asks the supervisor to reconnect immediately.
	Retry func()
}

// editorKind names the inline text entry currently open.
type editorKind int

const (
	editorNone editorKind = iota
	editorFilter
	editorLocalPort
)

// noSelection is the cursor value meaning "nothing highlighted yet".
const noSelection = -1

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

	// editor is the inline text entry shown in the bottom border.
	editor     editorKind
	editorPort int
	input      textInput
	query      string

	showHelp   bool
	showDetail bool
	confirming bool
	menu       viewMenu
	prefs      config.ViewPrefs

	status StatusMsg
	// everConnected gates the reconnect screen: the first connection happens
	// before the UI starts, so anything after it going down is an outage.
	everConnected bool
	toast         ToastMsg
	toastID       int
	hasToast      bool

	lastClickPort int
	lastClickAt   time.Time
	// footerZones maps clickable spans of the key bar, recorded as it renders
	// so a click always hits what is actually on screen.
	footerZones []zone

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
	prefs := ctrl.ViewPrefs()
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
		prefs:   prefs,
		sortKey: sortKeyNamed(prefs.Sort),
		reverse: prefs.Reverse,
		rows:    ctrl.States(),
		// Nothing is highlighted until you move: a selection bar on arrival
		// implies you already chose something.
		cursor: noSelection,
	}
}

// Init starts the refresh ticker.
func (m *Model) Init() tea.Cmd {
	return tick(refreshInterval, func(t time.Time) tea.Msg { return refreshMsg(t) })
}

func tick(d time.Duration, fn func(time.Time) tea.Msg) tea.Cmd {
	return tea.Tick(d, fn)
}

// editing reports whether an inline text entry is open.
func (m *Model) editing() bool { return m.editor != editorNone }

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
		if msg.State == Connected || msg.State == Probing {
			m.everConnected = true
		}
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

// handleMouse routes clicks over the whole interface: the column headers sort,
// the key bar runs its action, a row selects, the VIA cell cycles the
// protocol, a double click opens, and an open overlay swallows the click.
func (m *Model) handleMouse(msg tea.MouseMsg) tea.Cmd {
	if m.dissolving != nil {
		return nil
	}

	// Wheel scrolls the table regardless of where the pointer is.
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		if m.overlayOpen() {
			return nil
		}
		m.cursor--
		m.clampCursor()
		return nil
	case tea.MouseButtonWheelDown:
		if m.overlayOpen() {
			return nil
		}
		m.cursor++
		m.clampCursor()
		return nil
	}

	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return nil
	}

	// A click anywhere dismisses a detail or help overlay, the way clicking
	// outside a popover does everywhere else.
	if m.showDetail || m.showHelp {
		m.showDetail, m.showHelp = false, false
		return nil
	}
	if m.menu.open {
		if i := m.menuRowAt(msg.Y); i >= 0 {
			m.menu.cursor = i
			return m.activateMenuItem(i)
		}
		m.menu.open = false
		return nil
	}
	// The quit confirmation is deliberately modal: it must be answered.
	if m.confirming {
		return nil
	}
	// A click outside an open editor commits it rather than silently editing
	// something else.
	if m.editing() {
		if m.editor == editorLocalPort {
			return m.applyLocalPort()
		}
		m.closeEditor()
		return nil
	}

	// The key bar is clickable, which makes every action discoverable without
	// knowing a single shortcut.
	if m.isFooterRow(msg.Y) {
		for _, z := range m.footerZones {
			if z.contains(msg.X) {
				return m.runAction(z.id)
			}
		}
		return nil
	}

	// Clicking a column header sorts by it, and clicking it again reverses.
	if msg.Y == rowColHeader {
		if c, ok := m.columnAt(msg.X); ok && c.sortable {
			if m.sortKey == c.sort {
				m.reverse = !m.reverse
			} else {
				m.sortKey, m.reverse = c.sort, false
			}
			m.reload()
			return m.showToast(ToastMsg{Text: "sort: " + m.sortKey.String()})
		}
		return nil
	}

	idx := m.rowAt(msg.Y)
	if idx < 0 {
		return nil
	}
	m.cursor = idx
	m.clampCursor()

	// The M and VIA cells are controls, not just readouts.
	if c, ok := m.columnAt(msg.X); ok {
		switch {
		case c.mode:
			m.lastClickPort = 0
			return m.cycleMode()
		case c.scheme:
			m.lastClickPort = 0
			return m.cycleScheme()
		}
	}

	now := m.now()
	port := m.selectedPort()
	isDouble := port == m.lastClickPort && now.Sub(m.lastClickAt) < doubleClickWindow
	m.lastClickPort, m.lastClickAt = port, now

	if !isDouble {
		return nil
	}
	m.lastClickPort = 0 // a third click starts a new pair
	return m.openSelected()
}

// overlayOpen reports whether a modal layer is covering the table.
func (m *Model) overlayOpen() bool {
	return m.confirming || m.showHelp || m.showDetail || m.editing() || m.menu.open
}

// isFooterRow reports whether y is the key bar.
func (m *Model) isFooterRow(y int) bool {
	return y == m.height-1
}

// runAction performs the action a key bar entry names, so mouse and keyboard
// share one implementation.
func (m *Model) runAction(id string) tea.Cmd {
	switch id {
	case "↑↓":
		return nil
	case "esc":
		m.confirming = true
		return nil
	case "enter":
		if _, ok := m.selected(); ok {
			m.showDetail = !m.showDetail
		}
	case "o":
		return m.openSelected()
	case "t":
		return m.cycleScheme()
	case "/":
		m.openFilter()
	case "l":
		return m.openLocalPort()
	case "c":
		m.menu.open = true
	case "a":
		return m.cycleMode()
	case "y":
		return m.copySelected()
	case "s":
		m.sortKey = m.sortKey.Next()
		m.reload()
		return m.showToast(ToastMsg{Text: "sort: " + m.sortKey.String()})
	case "?":
		m.showHelp = !m.showHelp
	}
	return nil
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
	if !m.prefs.ShowPreexisting {
		rows = hidePreexisting(rows)
	}
	rows = filterStates(rows, m.query)
	sortStates(rows, m.sortKey, m.reverse, m.prefs.InactiveLast)
	m.rows = rows

	if selected > 0 {
		m.cursor = noSelection
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

// move steps the selection, starting it at the first row if nothing is
// highlighted yet.
func (m *Model) move(delta int) {
	if len(m.rows) == 0 {
		m.cursor = noSelection
		return
	}
	// Once a row is highlighted, moving stays within the table: stepping off
	// the top should stop at the first row, not fall back to no selection.
	target := 0
	if m.cursor == noSelection {
		if delta < 0 {
			target = len(m.rows) - 1
		}
	} else {
		target = m.cursor + delta
	}
	if target < 0 {
		target = 0
	}
	if target >= len(m.rows) {
		target = len(m.rows) - 1
	}
	m.cursor = target
	m.clampCursor()
}

func (m *Model) selected() (tunnel.State, bool) {
	if m.cursor >= 0 && m.cursor < len(m.rows) {
		return m.rows[m.cursor], true
	}
	return tunnel.State{}, false
}

func (m *Model) clampCursor() {
	if len(m.rows) == 0 {
		m.cursor, m.offset = noSelection, 0
		return
	}
	if m.cursor == noSelection {
		m.offset = 0
		return
	}
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
	if m.editing() {
		return m.handleEditorKey(msg)
	}
	if m.menu.open {
		return m.handleMenuKey(msg)
	}
	if m.offline() {
		if cmd, handled := m.handleOfflineKey(msg); handled {
			return cmd
		}
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

// handleOfflineKey handles the reconnect screen. Only the keys it owns are
// claimed; quitting and the rest of the table still work behind it.
func (m *Model) handleOfflineKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	if msg.String() != "r" {
		return nil, false
	}
	if m.opts.Retry == nil {
		return nil, true
	}
	m.opts.Retry()
	m.status.NextRetry = time.Time{}
	return m.showToast(ToastMsg{Text: "reconnecting…"}), true
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

func (m *Model) handleEditorKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "ctrl+c":
		kind := m.editor
		m.closeEditor()
		if kind == editorFilter {
			m.query = ""
			m.reload()
		}
		return nil
	case "enter":
		if m.editor == editorLocalPort {
			return m.applyLocalPort()
		}
		m.closeEditor()
		return nil
	}
	if m.input.Update(msg) && m.editor == editorFilter {
		m.query = m.input.Value()
		m.cursor = noSelection
		m.reload()
	}
	return nil
}

// closeEditor dismisses the inline editor.
func (m *Model) closeEditor() {
	m.editor = editorNone
	m.editorPort = 0
	m.input.Reset()
}

// openFilter starts a search.
func (m *Model) openFilter() {
	m.editor = editorFilter
	m.input.SetValue(m.query)
}

// openLocalPort starts editing the selected row's local port.
func (m *Model) openLocalPort() tea.Cmd {
	st, ok := m.selected()
	if !ok {
		return m.needSelection()
	}
	m.editor = editorLocalPort
	m.editorPort = st.RemotePort
	if st.PinnedLocal > 0 {
		m.input.SetValue(itoa(st.PinnedLocal))
	} else {
		m.input.SetValue("")
	}
	return nil
}

// applyLocalPort commits the local-port editor.
func (m *Model) applyLocalPort() tea.Cmd {
	text := strings.TrimSpace(m.input.Value())
	port := m.editorPort
	m.closeEditor()

	local := 0
	if text != "" {
		n, err := strconv.Atoi(text)
		if err != nil || n < 1 || n > 65535 {
			return m.showToast(ToastMsg{Text: "not a port number: " + text, Bad: true})
		}
		local = n
	}
	if err := m.ctrl.SetLocalPort(port, local); err != nil {
		m.reload()
		return m.showToast(ToastMsg{Text: err.Error(), Bad: true})
	}
	m.reload()
	if local == 0 {
		return m.showToast(ToastMsg{Text: "remote " + itoa(port) + " back to its default local port"})
	}
	return m.showToast(ToastMsg{Text: "remote " + itoa(port) + " pinned to local " + itoa(local) + " (remembered)"})
}

// needSelection nudges the user when an action requires a highlighted row.
func (m *Model) needSelection() tea.Cmd {
	if len(m.rows) == 0 {
		return nil
	}
	return m.showToast(ToastMsg{Text: "pick a row first — ↑↓ or click", Bad: true})
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
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "pgup", "ctrl+u":
		m.move(-m.tableHeight())
	case "pgdown", "ctrl+d":
		m.move(m.tableHeight())
	case "home", "g":
		m.cursor = 0
		m.clampCursor()
	case "end", "G":
		m.cursor = len(m.rows) - 1
		m.clampCursor()

	case "s":
		m.sortKey = m.sortKey.Next()
		m.savePrefs()
		m.reload()
		return m.showToast(ToastMsg{Text: "sort: " + m.sortKey.String()})
	case "r":
		m.reverse = !m.reverse
		m.savePrefs()
		m.reload()

	case "/":
		m.openFilter()
		return nil
	case "l":
		return m.openLocalPort()

	case "?":
		m.showHelp = !m.showHelp
	case "enter", "d":
		if _, ok := m.selected(); ok {
			m.showDetail = !m.showDetail
		}

	case "a":
		return m.cycleMode()
	case "t":
		return m.cycleScheme()
	case " ", "o":
		return m.openSelected()
	case "y":
		return m.copySelected()
	case "p":
		return m.togglePause()
	case "c":
		m.menu.open = true
	}
	return nil
}

// cycleMode steps the selected port through auto → on → off.
func (m *Model) cycleMode() tea.Cmd {
	st, ok := m.selected()
	if !ok {
		return m.needSelection()
	}
	mode := m.ctrl.CycleMode(st.RemotePort)
	m.reload()

	switch mode {
	case config.ModeOn:
		return m.showToast(ToastMsg{Text: "remote " + itoa(st.RemotePort) + ": always forward (remembered)"})
	case config.ModeOff:
		return m.showToast(ToastMsg{Text: "remote " + itoa(st.RemotePort) + ": never forward (remembered)"})
	default:
		return m.showToast(ToastMsg{Text: "remote " + itoa(st.RemotePort) + ": back to automatic"})
	}
}

// cycleScheme steps the selected row through unknown → http → https.
func (m *Model) cycleScheme() tea.Cmd {
	st, ok := m.selected()
	if !ok {
		return m.needSelection()
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
		return m.needSelection()
	}
	// Opening an unidentified port means guessing at its protocol, and a wrong
	// guess sends a plaintext request at something that may not want one. Ask
	// instead: it is one keystroke, and the answer is remembered.
	if st.Scheme == tunnel.SchemeUnknown {
		return m.showToast(ToastMsg{
			Text: "remote " + itoa(st.RemotePort) + ": press t to say http or https first",
			Bad:  true,
		})
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
		return m.needSelection()
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
