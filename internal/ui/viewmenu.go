package ui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// viewMenu is the popup behind `c`: this host's settings, and nothing else.
// Every option here changes what you look at, never what is forwarded — that
// separation is the point of the menu existing, since a "view" toggle that
// quietly opened tunnels is exactly the surprise it was built to remove.
type viewMenu struct {
	open   bool
	cursor int
}

// menuItem is one row of the view menu.
type menuItem struct {
	// toggle items render a checkbox; choice items render their options.
	toggle  bool
	label   string
	help    string
	choices []menuChoice
}

type menuChoice struct {
	label string
	sort  SortKey
}

// menuItems is the menu's contents, in order.
var menuItems = []menuItem{
	{toggle: true, label: "show pre-existing", help: "services already running when autotun connected"},
	{toggle: true, label: "inactive last", help: "sink rows with no tunnel below the live ones"},
	{label: "sort by", choices: []menuChoice{
		{label: "port", sort: SortRemote},
		{label: "recent", sort: SortAge},
		{label: "traffic", sort: SortTraffic},
		{label: "process", sort: SortProcess},
	}},
}

// menuRows is how many rows the menu's items occupy.
func menuRows() int { return len(menuItems) }

// value reports the current setting for a menu row.
func (m *Model) menuValue(i int) bool {
	switch i {
	case 0:
		return m.prefs.ShowPreexisting
	case 1:
		return m.prefs.InactiveLast
	}
	return false
}

// sortChoice returns the index of the active sort within the choice list, or
// -1 when the current sort is one only `s` can reach.
func (m *Model) sortChoice() int {
	for i, c := range menuItems[2].choices {
		if c.sort == m.sortKey {
			return i
		}
	}
	return -1
}

// activateMenuItem applies the row under the cursor.
func (m *Model) activateMenuItem(i int) tea.Cmd {
	switch i {
	case 0:
		m.prefs.ShowPreexisting = !m.prefs.ShowPreexisting
	case 1:
		m.prefs.InactiveLast = !m.prefs.InactiveLast
	case 2:
		choices := menuItems[2].choices
		next := (m.sortChoice() + 1) % len(choices)
		m.sortKey = choices[next].sort
	}
	m.savePrefs()
	m.reload()
	return nil
}

// savePrefs pushes the current presentation settings back to the controller,
// which persists them for this host.
func (m *Model) savePrefs() {
	m.prefs.Sort = m.sortKey.String()
	m.prefs.Reverse = m.reverse
	m.ctrl.SetViewPrefs(m.prefs)
}

// handleMenuKey routes a key press while the view menu is open.
func (m *Model) handleMenuKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "c", "q":
		m.menu.open = false
		return nil
	case "up", "k":
		if m.menu.cursor > 0 {
			m.menu.cursor--
		}
		return nil
	case "down", "j":
		if m.menu.cursor < menuRows()-1 {
			m.menu.cursor++
		}
		return nil
	case " ", "enter", "right", "l":
		return m.activateMenuItem(m.menu.cursor)
	}
	return nil
}

// menuRowAt maps a terminal row to a menu item, or -1 if the click missed one.
// The menu is centered, and each item occupies two rows: its own and the help
// line beneath it.
func (m *Model) menuRowAt(y int) int {
	lines := strings.Count(m.menuBox(), "\n") + 1
	top := (m.height - lines) / 2
	if top < 0 {
		top = 0
	}
	// Border, title, subtitle and the blank line beneath them.
	first := top + 4
	for i := range menuItems {
		if y == first+i*2 {
			return i
		}
	}
	return -1
}

// menuBox renders the view menu.
func (m *Model) menuBox() string {
	t := m.th

	var b strings.Builder
	b.WriteString(t.BoxTitle.Render("Settings") + t.Faintest.Render("  ·  remembered for this host") + "\n")
	b.WriteString(t.Faintest.Render("what is listed and how — nothing here forwards a port") + "\n\n")

	for i, item := range menuItems {
		selected := i == m.menu.cursor

		var line string
		if item.toggle {
			mark := " "
			if m.menuValue(i) {
				mark = "×"
			}
			line = "[" + mark + "] " + pad(item.label, 20)
		} else {
			line = "    " + pad(item.label, 20)
			var opts []string
			for j, c := range item.choices {
				if j == m.sortChoice() {
					opts = append(opts, t.Good.Render("("+c.label+")"))
				} else {
					opts = append(opts, t.Faintest.Render(" "+c.label+" "))
				}
			}
			line += strings.Join(opts, " ")
		}

		if selected {
			b.WriteString(t.Key.Render("▸ ") + t.Value.Render(line))
		} else {
			b.WriteString("  " + t.Row.Render(line))
		}
		b.WriteString("\n")
		if selected && item.help != "" {
			b.WriteString("    " + t.Faintest.Render(item.help) + "\n")
		} else {
			b.WriteString("\n")
		}
	}

	b.WriteString(t.Faintest.Render("↑↓ move · space change · esc close"))
	return t.Box.Render(b.String())
}
