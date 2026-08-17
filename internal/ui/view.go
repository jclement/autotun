package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/jclement/autotun/internal/tunnel"
)

// Column widths. PROCESS absorbs whatever is left over.
const (
	wMark   = 1
	wLocal  = 6
	wArrow  = 1
	wRemote = 6
	wScheme = 5
	wAge    = 5
	wConns  = 5
	wBytes  = 8
	gap     = 2
	minProc = 12

	headerLines = 3 // title, blank, column header
	footerLines = 2 // blank, keybar
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
	}
	return m.pendingOSC + view
}

// baseView is the app without any overlay, and the frame the dissolve captures.
func (m *Model) baseView() string {
	var b strings.Builder
	b.WriteString(m.headerView())
	b.WriteByte('\n')
	b.WriteString(m.tableView())
	b.WriteByte('\n')
	b.WriteString(m.footerView())
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

func (m *Model) headerView() string {
	t := m.th
	title := t.Title.Render("autotun")
	sep := t.Separator.Render(" ▸ ")
	host := t.Host.Render(m.opts.Host)

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

	var right []string
	right = append(right, m.statusChip())
	// Paused is a mode, not an event, so it lives in the header where a
	// transient footer toast can never hide it.
	if m.ctrl.Policy().Paused {
		right = append(right, t.Warning.Render("PAUSED"))
	}
	right = append(right, t.Good.Render(fmt.Sprintf("%d tunnel%s", active, plural(active))))
	if skipped > 0 {
		right = append(right, t.Meta.Render(fmt.Sprintf("%d idle", skipped)))
	}
	if failed > 0 {
		right = append(right, t.Bad.Render(fmt.Sprintf("%d failed", failed)))
	}
	right = append(right, t.Meta.Render(FormatUptime(m.now().Sub(m.started))))

	left := title + sep + host
	rightStr := strings.Join(right, t.Separator.Render(" · "))

	line := fitLine(left, rightStr, m.width)

	cols := t.Header.Render(m.columnHeader())
	return line + "\n\n" + cols
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

func (m *Model) procWidth() int {
	used := wMark + 1 + wLocal + gap + wArrow + gap + wRemote + gap + wScheme + gap +
		wAge + gap + wConns + gap + wBytes + gap + wBytes
	w := m.width - used - gap - 1
	if w < minProc {
		return minProc
	}
	return w
}

func (m *Model) columnHeader() string {
	cols := []string{
		padLeft("LOCAL", wLocal),
		strings.Repeat(" ", wArrow),
		padLeft("REMOTE", wRemote),
		pad("VIA", wScheme),
		pad("PROCESS", m.procWidth()),
		padLeft("AGE", wAge),
		padLeft("CONNS", wConns),
		padLeft("IN", wBytes),
		padLeft("OUT", wBytes),
	}
	prefix := strings.Repeat(" ", wMark+1)
	return clampWidth(prefix+strings.Join(cols, strings.Repeat(" ", gap)), m.width)
}

func (m *Model) tableView() string {
	h := m.tableHeight()
	if len(m.rows) == 0 {
		return m.emptyView(h)
	}

	var b strings.Builder
	end := m.offset + h
	if end > len(m.rows) {
		end = len(m.rows)
	}
	for i := m.offset; i < end; i++ {
		b.WriteString(m.rowView(m.rows[i], i == m.cursor))
		b.WriteByte('\n')
	}
	for i := end - m.offset; i < h; i++ {
		b.WriteByte('\n')
	}
	return strings.TrimSuffix(b.String(), "\n")
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
		if i == mid {
			lines[i] = lipgloss.PlaceHorizontal(m.width, lipgloss.Center, t.Faintest.Render(msg))
		}
	}
	return strings.Join(lines, "\n")
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
	line = clampWidth(line, m.width)

	if selected {
		// Pad the selection to the full width so the highlight is a bar.
		if w := ansi.StringWidth(line); w < m.width {
			line += strings.Repeat(" ", m.width-w)
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

	// Non-forwarding rows say why, in place of the scheme and traffic columns.
	if s.Status != tunnel.StatusActive {
		reason := string(s.Skip)
		if s.Status == tunnel.StatusError {
			reason = s.Err
		}
		if s.Manual && reason == "" {
			reason = "detached"
		}
		return prefix + padLeft(local, wLocal) + strings.Repeat(" ", gap) + arrow +
			strings.Repeat(" ", gap) + padLeft(remote, wRemote) + strings.Repeat(" ", gap) +
			pad("", wScheme) + strings.Repeat(" ", gap) +
			pad(cmd, m.procWidth()) + strings.Repeat(" ", gap) + reason
	}

	conns := itoa(s.ActiveConns)
	cols := []string{
		padLeft(local, wLocal),
		arrow,
		padLeft(remote, wRemote),
		pad(s.Scheme.Label(), wScheme),
		pad(cmd, m.procWidth()),
		padLeft(FormatAge(now.Sub(s.Created)), wAge),
		padLeft(conns, wConns),
		padLeft(FormatBytes(s.BytesIn), wBytes),
		padLeft(FormatBytes(s.BytesOut), wBytes),
	}
	return prefix + strings.Join(cols, strings.Repeat(" ", gap))
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

func (m *Model) footerView() string {
	t := m.th
	if m.filtering {
		return "\n " + t.Key.Render("filter") + t.Meta.Render(" ▸ ") + m.filter.Render(t) +
			t.Faintest.Render("   enter to keep · esc to clear")
	}
	if m.hasToast {
		style := t.Good
		if m.toast.Bad {
			style = t.Bad
		}
		return "\n " + style.Render("▸ "+m.toast.Text)
	}

	// Ordered by how much you would miss it: whatever does not fit is dropped
	// from the end, a whole hint at a time. Truncating mid-word would leave
	// something like "esc q", which reads as a different key.
	keys := [][2]string{
		{"↑↓", "move"}, {"esc", "quit"}, {"enter", "detail"}, {"o", "open"},
		{"t", "http/s"}, {"/", "filter"}, {"a", "attach"}, {"y", "copy"},
		{"s", "sort"}, {"?", "help"},
	}

	var flags []string
	if m.query != "" {
		flags = append(flags, t.Meta.Render("/"+m.query))
	}
	if m.reverse {
		flags = append(flags, t.Meta.Render("↑"+m.sortKey.String()))
	} else if m.sortKey != SortRemote {
		flags = append(flags, t.Meta.Render("↓"+m.sortKey.String()))
	}
	right := strings.Join(flags, t.Faintest.Render(" · "))

	// Reserve room for the right-hand indicators plus the separating space.
	budget := m.width - 1
	if right != "" {
		budget -= ansi.StringWidth(right) + 2
	}

	sep := t.Faintest.Render(" · ")
	var bar strings.Builder
	used := 0
	for i, k := range keys {
		hint := t.Key.Render(k[0]) + " " + t.KeyDesc.Render(k[1])
		width := ansi.StringWidth(k[0]) + 1 + ansi.StringWidth(k[1])
		if i > 0 {
			width += 3 // " · "
		}
		if used+width > budget {
			break
		}
		if i > 0 {
			bar.WriteString(sep)
		}
		bar.WriteString(hint)
		used += width
	}

	return "\n" + fitLine(" "+bar.String(), right, m.width)
}

// fitLine puts left and right on one line of the given width, dropping the
// right-hand side when there is no room for it.
func fitLine(left, right string, width int) string {
	lw, rw := ansi.StringWidth(left), ansi.StringWidth(right)
	if width <= 0 {
		return left
	}
	if rw == 0 {
		return clampWidth(left, width)
	}
	if lw+rw+2 > width {
		if lw > width {
			return clampWidth(left, width)
		}
		return left
	}
	return left + strings.Repeat(" ", width-lw-rw-1) + right + " "
}

func (m *Model) confirmBox() string {
	t := m.th
	body := t.BoxTitle.Render("Close all tunnels and quit?") + "\n\n" +
		t.Meta.Render("Local ports will be released immediately.") + "\n\n" +
		t.Key.Render("y") + t.KeyDesc.Render(" yes") + t.Faintest.Render("   ·   ") +
		t.Key.Render("n") + t.KeyDesc.Render(" keep running")
	return t.Box.Render(body)
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
		return t.Label.Render(pad(label, 12)) + t.Value.Render(value) + "\n"
	}

	var b strings.Builder
	b.WriteString(t.BoxTitle.Render(fmt.Sprintf("remote :%d", s.RemotePort)) + "\n\n")
	if s.Status == tunnel.StatusActive {
		b.WriteString(row("url", s.URL()))
		b.WriteString(row("local", fmt.Sprintf("%s:%d", s.LocalAddr, s.LocalPort)))
		if s.Remapped {
			b.WriteString(row("", t.Warning.Render("remapped — the remote port was busy locally")))
		}
	}
	b.WriteString(row("status", string(s.Status)))
	if s.Skip != "" {
		b.WriteString(row("reason", string(s.Skip)))
	}
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
	t := m.th
	sections := []struct {
		title string
		keys  [][2]string
	}{
		{"navigate", [][2]string{
			{"↑ ↓ / j k", "move"},
			{"g / G", "first / last"},
			{"pgup / pgdn", "page"},
			{"/", "filter by port or process"},
			{"s / r", "cycle sort / reverse"},
		}},
		{"act", [][2]string{
			{"enter, d", "detail"},
			{"o", "open in browser"},
			{"t", "set http / https (remembered)"},
			{"space, 2×click", "open, if http or https"},
			{"y", "copy URL to clipboard"},
			{"a", "attach / detach this port"},
			{"e", "toggle pre-existing ports"},
			{"p", "pause automatic forwarding"},
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
			b.WriteString("  " + t.Key.Render(pad(k[0], 12)) + t.KeyDesc.Render(k[1]) + "\n")
		}
	}
	b.WriteString("\n" + t.Faintest.Render("? or esc to close"))
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

var _ = time.Now
