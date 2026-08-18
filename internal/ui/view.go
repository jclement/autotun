package ui

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/jclement/autotun/internal/config"
	"github.com/jclement/autotun/internal/tunnel"
)

// Column widths. PROCESS absorbs whatever is left over.
const (
	wMark = 1
	wMode = 1
	// The port columns carry a sort indicator on top of their title, so they
	// are one cell wider than "REMOTE" needs on its own.
	wLocal  = 7
	wArrow  = 1
	wRemote = 7
	wScheme = 7 // fits "unknown"
	wAge    = 5
	wConns  = 6
	wBytes  = 8
	gap     = 2
	minProc = 11
	// A very wide terminal should not stretch PROCESS across the whole screen:
	// that strands AGE/CONNS/IN/OUT far from the row they describe and leaves a
	// canyon of whitespace in between. Past this the table just stops growing.
	maxProc = 56
)

// Fixed rows of the frame, used for both drawing and mouse hit-testing.
const (
	rowColHeader = 1
	rowFirstData = 3

	headerLines = rowFirstData
	footerLines = 1 // the bottom border carries the key bar
)

// Box-drawing pieces.
const (
	cornerTL = "╭"
	cornerTR = "╮"
	cornerBL = "╰"
	cornerBR = "╯"
	edgeH    = "─"
	edgeV    = "│"
	teeL     = "├"
	teeR     = "┤"
)

// Recency windows for the activity marker. These are what make a busy table
// scannable: the thing you just started, and the thing you are actually using,
// should be findable without reading every row.
const (
	freshWindow = 20 * time.Second // recently created
	liveWindow  = 5 * time.Second  // recently carried traffic
)

// activity classifies a row for highlighting.
type activity int

const (
	activityNone activity = iota
	activityFresh
	activityLive
)

// activityOf reports whether a row is new or currently in use.
func activityOf(s tunnel.State, now time.Time) activity {
	if s.Status != tunnel.StatusActive {
		return activityNone
	}
	if s.ActiveConns > 0 || (!s.LastByte.IsZero() && now.Sub(s.LastByte) < liveWindow) {
		return activityLive
	}
	if !s.Created.IsZero() && now.Sub(s.Created) < freshWindow {
		return activityFresh
	}
	return activityNone
}

// View renders the current frame.
func (m *Model) View() string {
	if m.quit {
		// Leave the terminal clean on exit.
		return ""
	}
	if m.dissolving != nil {
		return m.pendingOSC + m.dissolving.View()
	}

	view := m.baseView()
	switch {
	case m.confirming:
		view = overlayCenter(view, m.confirmBox(), m.width, m.height)
	case m.showHelp:
		view = overlayCenter(view, m.helpBox(), m.width, m.height)
	case m.showDetail:
		view = overlayCenter(view, m.detailBox(), m.width, m.height)
	case m.protocolPrompt:
		view = overlayCenter(view, m.protocolBox(), m.width, m.height)
	case m.menu.open:
		view = overlayCenter(view, m.menuBox(), m.width, m.height)
	case m.offline():
		view = overlayCenter(view, m.reconnectBox(), m.width, m.height)
	}
	return m.pendingOSC + view
}

// inner is the width available between the frame's side borders.
func (m *Model) inner() int {
	if m.width < 4 {
		return 1
	}
	return m.width - 2
}

// baseView is the app without any overlay, and the frame the dissolve captures.
func (m *Model) baseView() string {
	var b strings.Builder
	b.WriteString(m.topBorder())
	b.WriteByte('\n')
	b.WriteString(m.boxLine(m.th.Header.Render(m.columnHeader())))
	b.WriteByte('\n')
	b.WriteString(m.separator())
	b.WriteByte('\n')
	b.WriteString(m.tableView())
	b.WriteByte('\n')
	b.WriteString(m.bottomBorder())
	return b.String()
}

// boxLine wraps content in the frame's side borders, padding it to fit.
func (m *Model) boxLine(content string) string {
	edge := m.th.Frame.Render(edgeV)
	w := ansi.StringWidth(content)
	if w > m.inner() {
		content = ansi.Truncate(content, m.inner(), "")
		w = ansi.StringWidth(content)
	}
	return edge + content + strings.Repeat(" ", m.inner()-w) + edge
}

// rule draws a horizontal run of the frame's edge.
func (m *Model) rule(n int) string {
	if n <= 0 {
		return ""
	}
	return m.th.Frame.Render(strings.Repeat(edgeH, n))
}

// borderWith composes a top or bottom border carrying a left and right label.
// Labels are dropped rather than truncated when the frame is too narrow.
func (m *Model) borderWith(left, right, lc, rc string) string {
	t := m.th
	lw, rw := ansi.StringWidth(left), ansi.StringWidth(right)
	if lw > 0 {
		lw += 2 // the spaces either side of the label
	}
	if rw > 0 {
		rw += 2
	}

	// Two corners plus one edge segment beside each.
	const fixed = 4
	if fixed+lw+rw > m.width {
		right, rw = "", 0
	}
	if fixed+lw > m.width {
		left, lw = "", 0
	}

	var b strings.Builder
	b.WriteString(t.Frame.Render(lc))
	b.WriteString(m.rule(1))
	if left != "" {
		b.WriteString(" " + left + " ")
	}
	b.WriteString(m.rule(m.width - fixed - lw - rw))
	if right != "" {
		b.WriteString(" " + right + " ")
	}
	b.WriteString(m.rule(1))
	b.WriteString(t.Frame.Render(rc))
	return b.String()
}

func (m *Model) topBorder() string {
	t := m.th
	title := t.Title.Render("autotun") + t.Separator.Render(" ▸ ") + t.Host.Render(m.opts.Host)
	status := m.statusLine()
	if exposedBind(m.opts.Bind) {
		// borderWith drops an overlong right label as a unit. A safety warning is
		// not optional decoration, so compact the summary before that can happen.
		need := 4 + ansi.StringWidth(title) + 2 + ansi.StringWidth(status) + 2
		if need > m.width {
			status = t.Bad.Render("LAN EXPOSED "+m.opts.Bind) +
				t.Separator.Render(" · ") + m.statusChip()
		}
		if 4+ansi.StringWidth(title)+2+ansi.StringWidth(status)+2 > m.width {
			status = t.Bad.Render("LAN EXPOSED " + m.opts.Bind)
		}
	}
	return m.borderWith(title, status, cornerTL, cornerTR)
}

func (m *Model) separator() string {
	t := m.th
	return t.Frame.Render(teeL) + m.rule(m.inner()) + t.Frame.Render(teeR)
}

func (m *Model) bottomBorder() string {
	if m.editing() {
		return m.borderWith(m.editorLabel(), "", cornerBL, cornerBR)
	}
	if m.hasToast {
		style := m.th.Good
		if m.toast.Bad {
			style = m.th.Bad
		}
		return m.borderWith(style.Render("▸ "+m.toast.Text), "", cornerBL, cornerBR)
	}
	return m.borderWith(m.keyBar(), m.viewChip(), cornerBL, cornerBR)
}

// statusLine is the connection and tunnel summary in the top border.
func (m *Model) statusLine() string {
	t := m.th

	active, skipped, failed := 0, 0, 0
	for _, r := range m.rows {
		switch r.Status {
		case tunnel.StatusActive:
			active++
		case tunnel.StatusError:
			failed++
		default:
			skipped++
		}
	}

	parts := []string{m.statusChip()}
	if m.ctrl.Policy().Paused {
		parts = append(parts, t.Warning.Render("PAUSED"))
	}
	if exposedBind(m.opts.Bind) {
		parts = append(parts, t.Bad.Render("LAN EXPOSED "+m.opts.Bind))
	}
	parts = append(parts, t.Good.Render(fmt.Sprintf("%d tunnel%s", active, plural(active))))
	if skipped > 0 {
		parts = append(parts, t.Meta.Render(fmt.Sprintf("%d idle", skipped)))
	}
	if failed > 0 {
		parts = append(parts, t.Bad.Render(fmt.Sprintf("%d failed", failed)))
	}
	parts = append(parts, t.Meta.Render(FormatUptime(m.now().Sub(m.started))))
	return strings.Join(parts, t.Separator.Render(" · "))
}

func exposedBind(bind string) bool {
	bind = strings.TrimSpace(bind)
	if bind == "" || strings.EqualFold(bind, "localhost") {
		return false
	}
	ip := net.ParseIP(strings.Trim(bind, "[]"))
	return ip == nil || !ip.IsLoopback()
}

// statusChip renders the connection indicator.
func (m *Model) statusChip() string {
	t := m.th
	switch m.status.State {
	case Connected:
		label := "connected"
		if m.status.Mode != "" {
			label += " (" + m.status.Mode + ")"
		}
		return t.Good.Render("● " + label)
	case Probing:
		return t.Warning.Render("◐ starting remote prober")
	case Reconnecting:
		s := "reconnecting"
		if m.status.Attempt > 0 {
			s += fmt.Sprintf(" #%d", m.status.Attempt)
		}
		return t.Warning.Render("◐ " + s)
	case Disconnected:
		return t.Bad.Render("○ disconnected")
	default:
		return t.Warning.Render("◌ connecting")
	}
}

// viewChip shows the active search and view mode in the bottom border.
func (m *Model) viewChip() string {
	t := m.th
	var parts []string
	if m.query != "" {
		parts = append(parts, t.Accent2Text.Render("/"+m.query))
	}
	if m.prefs.ShowPreexisting {
		parts = append(parts, t.Meta.Render("+pre-existing"))
	}
	return strings.Join(parts, t.Separator.Render(" · "))
}

type columnKind int

const (
	colLocal columnKind = iota
	colArrow
	colRemote
	colMode
	colScheme
	colProcess
	colAge
	colConns
	colIn
	colOut
)

// column is one table column's position and meaning. The renderer and the
// mouse hit-testing both read this, so a click always lands on the column the
// user actually sees.
type column struct {
	kind     columnKind
	title    string
	x        int // first cell, in terminal coordinates
	w        int
	right    bool    // right-aligned
	sort     SortKey // what clicking the header sorts by
	sortable bool
	scheme   bool // clicking a cell here cycles the protocol
	mode     bool // clicking a cell here cycles auto/on/off
}

// columns computes a responsive table layout. The mapping and service identity
// are always present; secondary metrics disappear as groups when the terminal
// narrows, instead of letting the right edge get blindly truncated.
func (m *Model) columns() []column {
	showMode, showScheme := true, true
	showAge, showConns, showTraffic := true, true, true

	build := func(procW int) []column {
		cols := []column{
			{kind: colLocal, title: "LOCAL", w: wLocal, right: true, sort: SortLocal, sortable: true},
			{kind: colArrow, w: wArrow},
			{kind: colRemote, title: "REMOTE", w: wRemote, right: true, sort: SortRemote, sortable: true},
		}
		if showMode {
			cols = append(cols, column{kind: colMode, title: "M", w: wMode, mode: true})
		}
		if showScheme {
			cols = append(cols, column{kind: colScheme, title: "VIA", w: wScheme, scheme: true})
		}
		cols = append(cols, column{kind: colProcess, title: "PROCESS", w: procW, sort: SortProcess, sortable: true})
		if showAge {
			cols = append(cols, column{kind: colAge, title: "AGE", w: wAge, right: true, sort: SortAge, sortable: true})
		}
		if showConns {
			cols = append(cols, column{kind: colConns, title: "CONNS", w: wConns, right: true, sort: SortConns, sortable: true})
		}
		if showTraffic {
			cols = append(cols,
				column{kind: colIn, title: "IN", w: wBytes, right: true, sort: SortTraffic, sortable: true},
				column{kind: colOut, title: "OUT", w: wBytes, right: true, sort: SortTraffic, sortable: true})
		}
		return cols
	}
	widthOf := func(cols []column) int {
		w := wMark + 1
		for i, c := range cols {
			if i > 0 {
				w += gap
			}
			w += c.w
		}
		return w
	}
	fits := func() bool { return widthOf(build(minProc)) <= m.inner() }
	if !fits() {
		showMode = false
	}
	if !fits() {
		showTraffic = false
	}
	if !fits() {
		showConns = false
	}
	if !fits() {
		showAge = false
	}
	if !fits() {
		showScheme = false
	}

	cols := build(minProc)
	extra := m.inner() - widthOf(cols)
	procW := minProc
	if extra > 0 {
		procW += extra
		if procW > maxProc {
			procW = maxProc
		}
		cols = build(procW)
	}

	x := 1 + wMark + 1 // left border, activity marker, space
	for i := range cols {
		cols[i].x = x
		x += cols[i].w + gap
	}
	return cols
}

func (m *Model) procWidth() int {
	for _, c := range m.columns() {
		if c.kind == colProcess {
			return c.w
		}
	}
	return minProc
}

// columnAt returns the column under a terminal x position.
func (m *Model) columnAt(x int) (column, bool) {
	for _, c := range m.columns() {
		if x >= c.x && x < c.x+c.w {
			return c, true
		}
	}
	return column{}, false
}

func (m *Model) columnHeader() string {
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", wMark+1))
	for i, c := range m.columns() {
		if i > 0 {
			b.WriteString(strings.Repeat(" ", gap))
		}
		title := c.title
		// Mark the active sort so the header explains the current ordering.
		if c.sortable && c.sort == m.sortKey {
			arrow := "↓"
			if m.reverse {
				arrow = "↑"
			}
			if c.right {
				title = arrow + title
			} else {
				title += arrow
			}
		}
		if c.right {
			b.WriteString(padLeft(title, c.w))
		} else {
			b.WriteString(pad(title, c.w))
		}
	}
	return b.String()
}

// tableHeight is how many data rows fit on screen.
func (m *Model) tableHeight() int {
	h := m.height - headerLines - footerLines
	if h < 1 {
		return 1
	}
	return h
}

func (m *Model) tableView() string {
	h := m.tableHeight()
	if len(m.rows) == 0 {
		return m.emptyView(h)
	}

	var lines []string
	end := m.offset + h
	if end > len(m.rows) {
		end = len(m.rows)
	}
	for i := m.offset; i < end; i++ {
		lines = append(lines, m.boxLine(m.rowView(m.rows[i], i == m.cursor)))
	}
	for len(lines) < h {
		lines = append(lines, m.boxLine(""))
	}
	return strings.Join(lines, "\n")
}

// emptyView explains the empty table rather than showing a blank rectangle.
func (m *Model) emptyView(h int) string {
	t := m.th
	var msg string
	switch {
	case m.query != "":
		msg = "nothing matches " + m.query
	case m.status.State != Connected:
		msg = "waiting for the connection…"
	default:
		msg = "no services yet — start something on the remote and it will appear here"
	}

	lines := make([]string, h)
	mid := h / 2
	for i := range lines {
		content := ""
		if i == mid {
			content = lipgloss.PlaceHorizontal(m.inner(), lipgloss.Center, t.Faintest.Render(msg))
		}
		lines[i] = m.boxLine(content)
	}
	return strings.Join(lines, "\n")
}

// modeGlyph renders a port's standing decision.
func modeGlyph(mode config.Mode) string {
	switch mode {
	case config.ModeOn:
		return "+"
	case config.ModeOff:
		return "-"
	default:
		return " "
	}
}

// rowView renders one service.
func (m *Model) rowView(s tunnel.State, selected bool) string {
	t := m.th
	now := m.now()

	local, arrow, remote := "—", " ", itoa(s.RemotePort)
	switch s.Status {
	case tunnel.StatusActive:
		local = itoa(s.LocalPort)
		if s.Remapped {
			arrow = "≠"
		} else {
			arrow = "←"
		}
	case tunnel.StatusError:
		arrow = "!"
	}

	cmd := s.Cmd
	if cmd == "" {
		cmd = "(unknown)"
	}

	act := activityOf(s, now)
	line := m.rowText(s, act, local, arrow, remote, cmd, now)
	line = clampWidth(line, m.inner())

	if selected {
		// Pad the selection to the full inner width so the highlight is a bar.
		if w := ansi.StringWidth(line); w < m.inner() {
			line += strings.Repeat(" ", m.inner()-w)
		}
		return t.RowSel.Render(ansi.Strip(line))
	}

	switch {
	case s.Status == tunnel.StatusError:
		return t.Bad.Render(line)
	case s.Status != tunnel.StatusActive:
		return t.Faintest.Render(line)
	case act == activityLive:
		return t.Live.Render(line)
	case act == activityFresh:
		return t.Fresh.Render(line)
	default:
		return t.Row.Render(line)
	}
}

// rowText lays out one row's columns without applying the row-level style.
func (m *Model) rowText(s tunnel.State, act activity, local, arrow, remote, cmd string, now time.Time) string {
	prefix := m.marker(act) + " "
	cols := m.columns()
	procAt := -1
	for i, c := range cols {
		if c.kind == colProcess {
			procAt = i
			break
		}
	}

	reason := ""
	switch s.Status {
	case tunnel.StatusError:
		reason = s.Err
	case tunnel.StatusOffline:
		reason = "offline"
	case tunnel.StatusSkipped:
		reason = string(s.Skip)
	}
	hasTail := procAt >= 0 && procAt < len(cols)-1

	var values []string
	for i, c := range cols {
		if reason != "" && hasTail && i > procAt {
			break
		}
		value := ""
		switch c.kind {
		case colLocal:
			value = local
		case colArrow:
			value = arrow
		case colRemote:
			value = remote
		case colMode:
			value = modeGlyph(s.Mode)
		case colScheme:
			value = s.Scheme.Label()
		case colProcess:
			value = displayProcess(s, cmd)
			if reason != "" && !hasTail {
				value = reason
			}
		case colAge:
			value = FormatAge(now.Sub(s.Created))
		case colConns:
			value = itoa(s.ActiveConns)
		case colIn:
			value = FormatBytes(s.BytesIn)
		case colOut:
			value = FormatBytes(s.BytesOut)
		}
		if c.right {
			values = append(values, padLeft(value, c.w))
		} else {
			values = append(values, pad(value, c.w))
		}
	}
	line := prefix + strings.Join(values, strings.Repeat(" ", gap))
	if reason != "" && hasTail {
		line += strings.Repeat(" ", gap) + reason
	}
	return line
}

func displayProcess(s tunnel.State, fallback string) string {
	if s.Label == "" {
		return fallback
	}
	if fallback == "" || fallback == "(unknown)" {
		return s.Label
	}
	return s.Label + " · " + fallback
}

// marker is the leading activity glyph: a filled dot for a tunnel carrying
// traffic, a hollow one for a tunnel that just appeared.
func (m *Model) marker(act activity) string {
	switch act {
	case activityLive:
		return "●"
	case activityFresh:
		return "◦"
	default:
		return " "
	}
}

// editorLabel renders the inline editor shown in the bottom border.
func (m *Model) editorLabel() string {
	t := m.th
	var prompt, hint string
	switch m.editor {
	case editorFilter:
		prompt, hint = "search", "enter to keep · esc to clear"
	case editorLocalPort:
		prompt = fmt.Sprintf("local port for remote %d", m.editorPort)
		hint = "blank resets · enter to apply · esc to cancel"
	case editorPortLabel:
		prompt = fmt.Sprintf("name for remote %d", m.editorPort)
		hint = "blank clears · enter to remember · esc to cancel"
	}
	return t.Key.Render(prompt) + t.Meta.Render(" ▸ ") + m.input.Render(t) +
		t.Faintest.Render("   "+hint)
}

// keyBar renders the clickable key hints, dropping whole entries that do not
// fit rather than truncating one mid-word.
func (m *Model) keyBar() string {
	t := m.th

	// Deliberately short. Everything else is one keystroke away behind ?,
	// and a key bar nobody can read is not a key bar.
	keys := [][2]string{
		{"↑↓", "move"}, {"enter", "detail"}, {"o", "open"},
		{"c", "config"}, {"?", "help"}, {"esc", "quit"},
	}

	// Reserve room for the right-hand chip plus the border decorations.
	budget := m.width - 6 - ansi.StringWidth(m.viewChip())

	sep := t.Faintest.Render(" · ")
	var bar strings.Builder
	used := 0
	m.footerZones = m.footerZones[:0]
	for i, k := range keys {
		hint := t.Key.Render(k[0]) + " " + t.KeyDesc.Render(k[1])
		width := ansi.StringWidth(k[0]) + 1 + ansi.StringWidth(k[1])
		lead := 0
		if i > 0 {
			lead = 3 // " · "
		}
		if used+lead+width > budget {
			break
		}
		if i > 0 {
			bar.WriteString(sep)
		}
		bar.WriteString(hint)
		// The bar starts after "╰─ ", three cells in.
		m.footerZones = append(m.footerZones, zone{
			x0: 3 + used + lead,
			x1: 3 + used + lead + width,
			id: k[0],
		})
		used += lead + width
	}
	return bar.String()
}

// zone is a clickable span on a single line.
type zone struct {
	x0, x1 int // half-open range of terminal columns
	id     string
}

// contains reports whether x falls inside the zone.
func (z zone) contains(x int) bool { return x >= z.x0 && x < z.x1 }

// offline reports whether the link has gone away since we were connected.
// The first connection happens before the UI starts, so a drop after that is
// an outage worth taking over the screen for.
func (m *Model) offline() bool {
	if !m.everConnected {
		return false
	}
	return m.status.State == Disconnected || m.status.State == Reconnecting
}

// reconnectBox is the waiting screen shown while the link is down.
//
// It says what is remembered, not just what is broken: the local assignments
// are retained and autotun tries to reclaim them after reconnecting.
func (m *Model) reconnectBox() string {
	t := m.th

	held := 0
	for _, r := range m.rows {
		if r.Status == tunnel.StatusOffline || r.Status == tunnel.StatusActive {
			held++
		}
	}

	var b strings.Builder
	if m.status.State == Reconnecting {
		b.WriteString(t.Warning.Render("◐  Reconnecting to " + m.opts.Host))
	} else {
		b.WriteString(t.Bad.Render("○  Connection lost"))
	}
	b.WriteString("\n\n")

	if detail := strings.TrimSpace(m.status.Detail); detail != "" {
		b.WriteString(t.Meta.Render(firstLine(detail)) + "\n\n")
	}

	if held > 0 {
		b.WriteString(t.Value.Render(fmt.Sprintf("Remembering %d tunnel assignment%s.", held, plural(held))) + "\n")
		b.WriteString(t.Faintest.Render("autotun will try to reclaim the same local port numbers.") + "\n\n")
	}

	switch {
	case m.status.State == Reconnecting:
		b.WriteString(t.Meta.Render("Trying now…"))
	case !m.status.NextRetry.IsZero():
		wait := m.status.NextRetry.Sub(m.now())
		if wait < 0 {
			wait = 0
		}
		b.WriteString(t.Meta.Render(fmt.Sprintf("Next attempt in %ds", int(wait.Seconds()+0.5))))
	default:
		b.WriteString(t.Meta.Render("Waiting…"))
	}
	if m.status.Attempt > 1 {
		b.WriteString(t.Faintest.Render(fmt.Sprintf("   (attempt %d)", m.status.Attempt)))
	}

	b.WriteString("\n\n")
	b.WriteString(t.Key.Render("r") + t.KeyDesc.Render(" try now") + t.Faintest.Render("   ·   ") +
		t.Key.Render("esc") + t.KeyDesc.Render(" quit"))

	return t.Box.Render(b.String())
}

// firstLine keeps a multi-line error readable inside the box.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func (m *Model) confirmBox() string {
	t := m.th
	body := t.BoxTitle.Render("Close all tunnels and quit?") + "\n\n" +
		t.Meta.Render("Local ports will be released immediately.") + "\n\n" +
		t.Key.Render("y") + t.KeyDesc.Render(" yes") + t.Faintest.Render("   ·   ") +
		t.Key.Render("n") + t.KeyDesc.Render(" keep running")
	return t.Box.Render(body)
}

func (m *Model) protocolBox() string {
	t := m.th
	s, ok := m.selected()
	if !ok {
		return ""
	}
	body := t.BoxTitle.Render(fmt.Sprintf("Open remote :%d", s.RemotePort)) + "\n\n" +
		t.Meta.Render("Choose the web protocol. This is remembered for this host and port.") + "\n\n" +
		t.Key.Render("enter / h") + t.KeyDesc.Render(" http") + t.Faintest.Render("   ·   ") +
		t.Key.Render("s") + t.KeyDesc.Render(" https") + t.Faintest.Render("   ·   ") +
		t.Key.Render("esc") + t.KeyDesc.Render(" cancel")
	return t.Box.Render(body)
}

func (m *Model) protocolChoiceAt(x, y int) tunnel.Scheme {
	box := m.protocolBox()
	lines := strings.Split(box, "\n")
	if len(lines) < 2 {
		return tunnel.SchemeUnknown
	}
	boxW := 0
	for _, line := range lines {
		if w := ansi.StringWidth(line); w > boxW {
			boxW = w
		}
	}
	left := (m.width - boxW) / 2
	top := (m.height - len(lines)) / 2
	if left < 0 {
		left = 0
	}
	if top < 0 {
		top = 0
	}
	choiceRow := len(lines) - 2
	if y != top+choiceRow {
		return tunnel.SchemeUnknown
	}
	plain := ansi.Strip(lines[choiceRow])
	for _, choice := range []struct {
		needle string
		scheme tunnel.Scheme
	}{
		{"enter / h http", tunnel.SchemeHTTP},
		{"s https", tunnel.SchemeHTTPS},
	} {
		start := strings.Index(plain, choice.needle)
		if start < 0 {
			continue
		}
		cellStart := ansi.StringWidth(plain[:start])
		cellEnd := cellStart + ansi.StringWidth(choice.needle)
		if x >= left+cellStart && x < left+cellEnd {
			return choice.scheme
		}
	}
	return tunnel.SchemeUnknown
}

func (m *Model) detailBox() string {
	t := m.th
	s, ok := m.selected()
	if !ok {
		return ""
	}
	row := func(label, value string) string {
		if value == "" {
			return ""
		}
		return t.Label.Render(pad(label, 13)) + t.Value.Render(value) + "\n"
	}

	var b strings.Builder
	b.WriteString(t.BoxTitle.Render(fmt.Sprintf("remote :%d", s.RemotePort)) + "\n\n")
	b.WriteString(row("name", s.Label))
	if s.Status == tunnel.StatusActive {
		if s.Scheme == tunnel.SchemeUnknown {
			b.WriteString(row("endpoint", s.Endpoint()))
		} else {
			b.WriteString(row("url", s.URL()))
		}
		b.WriteString(row("local", fmt.Sprintf("%s:%d", s.LocalAddr, s.LocalPort)))
		if s.Remapped {
			b.WriteString(row("", t.Warning.Render("remapped — the remote port was busy locally")))
		}
	}
	b.WriteString(row("status", string(s.Status)))
	if s.Skip != "" {
		b.WriteString(row("reason", string(s.Skip)))
	}
	b.WriteString(row("mode", string(s.Mode)))
	if s.PinnedLocal > 0 {
		b.WriteString(row("pinned to", fmt.Sprintf("local %d", s.PinnedLocal)))
	}
	b.WriteString(row("protocol", s.Scheme.Label()))
	b.WriteString(row("command", s.Cmd))
	if s.PID > 0 {
		b.WriteString(row("pid", itoa(s.PID)))
	}
	b.WriteString(row("bound to", s.Binds))
	b.WriteString(row("first seen", FormatAge(m.now().Sub(s.FirstSeen))+" ago"))
	if s.Status == tunnel.StatusActive {
		b.WriteString(row("open since", FormatAge(m.now().Sub(s.Created))))
		b.WriteString(row("connections", fmt.Sprintf("%d active · %d total", s.ActiveConns, s.TotalConns)))
		b.WriteString(row("traffic", fmt.Sprintf("%s in · %s out", FormatBytes(s.BytesIn), FormatBytes(s.BytesOut))))
		if !s.LastByte.IsZero() {
			b.WriteString(row("last byte", FormatAge(m.now().Sub(s.LastByte))+" ago"))
		}
	}
	if s.Preexisting {
		b.WriteString(row("note", "was already listening when autotun connected"))
	}
	if s.Err != "" {
		b.WriteString(row("error", t.Bad.Render(s.Err)))
	}
	b.WriteString("\n" + t.Faintest.Render("esc to close"))
	return t.Box.Render(strings.TrimRight(b.String(), "\n"))
}

func (m *Model) helpBox() string {
	if m.height < 36 {
		return m.compactHelpBox()
	}
	t := m.th
	sections := []struct {
		title string
		keys  [][2]string
	}{
		{"navigate", [][2]string{
			{"↑ ↓ / j k", "move"},
			{"g / G", "first / last"},
			{"/", "search by port, name or process"},
			{"s / r", "cycle sort / reverse"},
		}},
		{"a port", [][2]string{
			{"enter, d", "detail"},
			{"t", "say http or https (remembered)"},
			{"o, space", "open in browser — asks HTTP/HTTPS when needed"},
			{"a", "auto → on → off (remembered)"},
			{"l", "set the local port (remembered)"},
			{"n", "name this port (remembered)"},
			{"y", "copy URL, or host:port for raw TCP"},
		}},
		{"everything", [][2]string{
			{"c", "settings: what is listed and how"},
			{"p", "pause new automatic tunnels; current ones stay up"},
		}},
		{"mouse", [][2]string{
			{"click", "select a row"},
			{"double click", "open in browser"},
			{"click M / VIA", "cycle mode / protocol"},
			{"click protocol", "choose HTTP/HTTPS in the open popup"},
			{"click header", "sort by that column"},
			{"click keybar", "run that action"},
		}},
		{"leave", [][2]string{
			{"esc, q", "quit (asks first)"},
			{"ctrl+c", "quit immediately"},
		}},
	}

	var b strings.Builder
	b.WriteString(t.BoxTitle.Render("autotun " + m.opts.Version))
	b.WriteString("\n")
	for _, sec := range sections {
		b.WriteString("\n" + t.Meta.Render(sec.title) + "\n")
		for _, k := range sec.keys {
			b.WriteString("  " + t.Key.Render(pad(k[0], 14)) + t.KeyDesc.Render(k[1]) + "\n")
		}
	}
	b.WriteString("\n" + t.Faintest.Render("? or esc to close"))
	return t.Box.Render(b.String())
}

// compactHelpBox keeps the complete keyboard surface visible in the common
// 80×24 terminal instead of rendering a tall popup whose lower half is clipped.
func (m *Model) compactHelpBox() string {
	t := m.th
	row := func(keys, desc string) string {
		return "  " + t.Key.Render(pad(keys, 13)) + t.KeyDesc.Render(desc) + "\n"
	}
	var b strings.Builder
	b.WriteString(t.BoxTitle.Render("autotun "+m.opts.Version) + "\n")
	b.WriteString(t.Meta.Render("navigate") + "\n")
	b.WriteString(row("↑↓ / j k / gG", "move / first / last"))
	b.WriteString(row("/", "search port, name or process"))
	b.WriteString(row("s / r", "cycle sort / reverse"))
	b.WriteString(t.Meta.Render("a port") + "\n")
	b.WriteString(row("enter, d", "detail"))
	b.WriteString(row("o, space", "open in browser; asks HTTP/HTTPS"))
	b.WriteString(row("t", "cycle unknown / http / https"))
	b.WriteString(row("a", "auto / on / off"))
	b.WriteString(row("l / n", "local port / name"))
	b.WriteString(row("y", "copy URL or host:port"))
	b.WriteString(t.Meta.Render("everything") + "\n")
	b.WriteString(row("c / p", "settings / pause new tunnels"))
	b.WriteString(row("mouse", "rows, controls, headers and wheel"))
	b.WriteString(t.Meta.Render("leave") + "\n")
	b.WriteString(row("esc, q / ^C", "ask to quit / quit now"))
	b.WriteString(t.Faintest.Render("? or esc to close"))
	return t.Box.Render(b.String())
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// overlayCenter composites box over base, centered, preserving the styling of
// the content it covers on either side.
func overlayCenter(base, box string, width, height int) string {
	if box == "" {
		return base
	}
	boxLines := strings.Split(box, "\n")
	boxW := 0
	for _, l := range boxLines {
		if w := ansi.StringWidth(l); w > boxW {
			boxW = w
		}
	}
	x := (width - boxW) / 2
	y := (height - len(boxLines)) / 2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}

	baseLines := strings.Split(base, "\n")
	for len(baseLines) < y+len(boxLines) {
		baseLines = append(baseLines, "")
	}

	for i, bl := range boxLines {
		row := y + i
		left := ansi.Truncate(baseLines[row], x, "")
		if w := ansi.StringWidth(left); w < x {
			left += strings.Repeat(" ", x-w)
		}
		right := ansi.TruncateLeft(baseLines[row], x+ansi.StringWidth(bl), "")
		baseLines[row] = left + "\x1b[0m" + bl + "\x1b[0m" + right
	}
	return strings.Join(baseLines, "\n")
}
