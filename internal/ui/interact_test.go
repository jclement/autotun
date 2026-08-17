package ui

import (
	"encoding/base64"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jclement/autotun/internal/tunnel"
)

func TestTextInputEditing(t *testing.T) {
	var in textInput

	for _, r := range "hello" {
		in.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if in.Value() != "hello" {
		t.Fatalf("Value() = %q", in.Value())
	}

	in.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if in.Value() != "hell" {
		t.Errorf("after backspace = %q", in.Value())
	}

	// Insert in the middle.
	in.Update(tea.KeyMsg{Type: tea.KeyLeft})
	in.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'X'}})
	if in.Value() != "helXl" {
		t.Errorf("insert at cursor = %q, want helXl", in.Value())
	}

	in.Update(tea.KeyMsg{Type: tea.KeyHome})
	in.Update(tea.KeyMsg{Type: tea.KeyDelete})
	if in.Value() != "elXl" {
		t.Errorf("delete at start = %q", in.Value())
	}

	in.Update(tea.KeyMsg{Type: tea.KeyEnd})
	in.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	if in.Value() != "" {
		t.Errorf("ctrl+u should clear, got %q", in.Value())
	}
}

func TestTextInputSpaceAndWordDelete(t *testing.T) {
	var in textInput
	in.SetValue("one two three")

	in.Update(tea.KeyMsg{Type: tea.KeyCtrlW})
	if in.Value() != "one two " {
		t.Errorf("ctrl+w = %q, want %q", in.Value(), "one two ")
	}

	in.Update(tea.KeyMsg{Type: tea.KeySpace})
	if !strings.HasSuffix(in.Value(), " ") {
		t.Errorf("space should insert, got %q", in.Value())
	}
}

func TestTextInputIgnoresControlRunes(t *testing.T) {
	var in textInput
	in.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a', '\x00', 'b'}})
	if in.Value() != "ab" {
		t.Errorf("Value() = %q, want control characters dropped", in.Value())
	}
}

func TestTextInputBoundaries(t *testing.T) {
	var in textInput
	// Editing an empty buffer must not panic or move the cursor negative.
	for _, k := range []tea.KeyType{tea.KeyBackspace, tea.KeyDelete, tea.KeyLeft, tea.KeyRight, tea.KeyCtrlW} {
		in.Update(tea.KeyMsg{Type: k})
	}
	if in.Value() != "" || in.cursor != 0 {
		t.Errorf("empty input ended up as %q at cursor %d", in.Value(), in.cursor)
	}

	in.SetValue("ab")
	if in.cursor != 2 {
		t.Errorf("SetValue should place the cursor at the end, got %d", in.cursor)
	}
	in.Reset()
	if in.Value() != "" || in.cursor != 0 {
		t.Error("Reset did not clear the input")
	}
}

func TestTextInputRendersACursor(t *testing.T) {
	var in textInput
	in.SetValue("hi")
	if got := in.Render(DefaultTheme()); got == "" {
		t.Error("Render produced nothing")
	}
	// An unhandled key is reported as not consumed, so the model can act on it.
	if in.Update(tea.KeyMsg{Type: tea.KeyEsc}) {
		t.Error("esc should not be consumed by the input")
	}
}

func TestOSC52(t *testing.T) {
	got := OSC52("http://localhost:3000")
	if !strings.HasPrefix(got, "\x1b]52;c;") {
		t.Errorf("missing the OSC 52 introducer: %q", got)
	}
	if !strings.HasSuffix(got, "\x07") {
		t.Errorf("missing the string terminator: %q", got)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(got, "\x1b]52;c;"), "\x07")
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("the payload is not valid base64: %v", err)
	}
	if string(decoded) != "http://localhost:3000" {
		t.Errorf("decoded = %q", decoded)
	}
}

func TestOpenCommand(t *testing.T) {
	name, args := openCommand("http://localhost:3000")
	switch runtime.GOOS {
	case "darwin":
		if name != "open" || len(args) != 1 {
			t.Errorf("darwin opener = %q %v", name, args)
		}
	case "windows":
		if name != "cmd" || len(args) != 4 || args[2] != "" {
			t.Errorf("windows opener = %q %v; the empty title argument matters", name, args)
		}
	default:
		if name != "xdg-open" {
			t.Errorf("unix opener = %q", name)
		}
	}
}

func TestOpenURLRejectsNonHTTP(t *testing.T) {
	for _, u := range []string{"file:///etc/passwd", "javascript:alert(1)", "ssh://host", ""} {
		if err := OpenURL(u); err == nil {
			t.Errorf("OpenURL(%q) should have been refused", u)
		}
	}
}

func TestModelSchemeCycle(t *testing.T) {
	stub := newStub(row(3000, 3000, "node"))
	m := newModel(t, stub)

	send(m, "t")
	if got := stub.schemes[3000]; got != tunnel.SchemeHTTP {
		t.Errorf("after one t, scheme = %q, want http", got)
	}
	if !strings.Contains(m.toast.Text, "http") || !strings.Contains(m.toast.Text, "remembered") {
		t.Errorf("toast = %q, should confirm the choice is remembered", m.toast.Text)
	}

	send(m, "t")
	if got := stub.schemes[3000]; got != tunnel.SchemeHTTPS {
		t.Errorf("after two t, scheme = %q, want https", got)
	}

	send(m, "t")
	if got := stub.schemes[3000]; got != tunnel.SchemeUnknown {
		t.Errorf("after three t, scheme = %q, want unknown", got)
	}
}

func TestModelSchemeChangesTheOpenedURL(t *testing.T) {
	var opened []string
	stub := newStub(row(3000, 3000, "node"))
	m := New(stub, Options{
		Host: "devbox", Now: func() time.Time { return testNow },
		OpenURL: func(u string) error { opened = append(opened, u); return nil },
	})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.reload()

	send(m, "t", "t") // -> https
	send(m, "o")

	if len(opened) != 1 || opened[0] != "https://127.0.0.1:3000" {
		t.Errorf("opened = %v, want the https URL", opened)
	}
}

// Space is the safe opener: it refuses to guess when the protocol is unknown.
func TestModelSpaceRequiresAKnownScheme(t *testing.T) {
	var opened []string
	stub := newStub(row(3000, 3000, "node"))
	m := New(stub, Options{
		Host: "devbox", Now: func() time.Time { return testNow },
		OpenURL: func(u string) error { opened = append(opened, u); return nil },
	})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.reload()

	send(m, " ")
	if len(opened) != 0 {
		t.Errorf("space opened %v despite an unknown protocol", opened)
	}
	if !m.toast.Bad || !strings.Contains(m.toast.Text, "press t") {
		t.Errorf("toast = %q, should point at the t key", m.toast.Text)
	}

	send(m, "t", " ")
	if len(opened) != 1 || opened[0] != "http://127.0.0.1:3000" {
		t.Errorf("opened = %v, want the http URL once the scheme is set", opened)
	}
}

// mouse builds a left-button press at a row.
func mouseAt(y int) tea.MouseMsg {
	return tea.MouseMsg{
		X: 5, Y: y,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	}
}

func TestModelMouseSelectsARow(t *testing.T) {
	m := newModel(t, newStub(row(3000, 3000, "a"), row(4000, 4000, "b"), row(5000, 5000, "c")))

	m.Update(mouseAt(headerLines + 1)) // the second data row
	if got, _ := m.selected(); got.RemotePort != 4000 {
		t.Errorf("clicking row 2 selected %d, want 4000", got.RemotePort)
	}

	// Clicks in the header and below the table are ignored.
	m.Update(mouseAt(0))
	if got, _ := m.selected(); got.RemotePort != 4000 {
		t.Errorf("a header click moved the selection to %d", got.RemotePort)
	}
	m.Update(mouseAt(headerLines + 40))
	if got, _ := m.selected(); got.RemotePort != 4000 {
		t.Errorf("a click past the last row moved the selection to %d", got.RemotePort)
	}
}

func TestModelDoubleClickOpens(t *testing.T) {
	var opened []string
	stub := newStub(row(3000, 3000, "node"))

	now := testNow
	m := New(stub, Options{
		Host: "devbox", Now: func() time.Time { return now },
		OpenURL: func(u string) error { opened = append(opened, u); return nil },
	})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.reload()
	send(m, "t") // make the scheme known so opening is allowed

	m.Update(mouseAt(headerLines))
	if len(opened) != 0 {
		t.Fatal("a single click should not open a browser")
	}

	now = now.Add(100 * time.Millisecond)
	m.Update(mouseAt(headerLines))
	if len(opened) != 1 {
		t.Fatalf("a double click should open the browser, opened %v", opened)
	}

	// A third click starts a fresh pair rather than opening again.
	now = now.Add(100 * time.Millisecond)
	m.Update(mouseAt(headerLines))
	if len(opened) != 1 {
		t.Errorf("a third click re-opened the browser: %v", opened)
	}
}

func TestModelSlowDoubleClickDoesNotOpen(t *testing.T) {
	var opened []string
	stub := newStub(row(3000, 3000, "node"))

	now := testNow
	m := New(stub, Options{
		Host: "devbox", Now: func() time.Time { return now },
		OpenURL: func(u string) error { opened = append(opened, u); return nil },
	})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.reload()
	send(m, "t")

	m.Update(mouseAt(headerLines))
	now = now.Add(2 * time.Second)
	m.Update(mouseAt(headerLines))

	if len(opened) != 0 {
		t.Errorf("two slow clicks opened %v", opened)
	}
}

func TestModelMouseWheelScrolls(t *testing.T) {
	var rows []tunnel.State
	for i := 0; i < 30; i++ {
		rows = append(rows, row(3000+i, 3000+i, "svc"))
	}
	m := newModel(t, newStub(rows...))

	wheel := func(b tea.MouseButton) tea.MouseMsg {
		return tea.MouseMsg{Action: tea.MouseActionPress, Button: b}
	}
	for i := 0; i < 5; i++ {
		m.Update(wheel(tea.MouseButtonWheelDown))
	}
	if m.cursor != 5 {
		t.Errorf("cursor = %d after 5 wheel-downs, want 5", m.cursor)
	}
	for i := 0; i < 3; i++ {
		m.Update(wheel(tea.MouseButtonWheelUp))
	}
	if m.cursor != 2 {
		t.Errorf("cursor = %d after 3 wheel-ups, want 2", m.cursor)
	}
}

// The mouse must not act while a modal layer owns the screen.
func TestModelMouseIgnoredDuringModals(t *testing.T) {
	m := newModel(t, newStub(row(3000, 3000, "a"), row(4000, 4000, "b")))

	send(m, "esc") // confirmation open
	m.Update(mouseAt(headerLines + 1))
	if got, _ := m.selected(); got.RemotePort != 3000 {
		t.Error("a click moved the selection while the quit prompt was open")
	}
	send(m, "n")

	send(m, "/") // filter open
	m.Update(mouseAt(headerLines + 1))
	if got, _ := m.selected(); got.RemotePort != 3000 {
		t.Error("a click moved the selection while the filter was open")
	}
}
