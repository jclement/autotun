package ui

import (
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jclement/autotun/internal/tunnel"
)

// stubController is a Controller backed by a fixed slice of rows.
type stubController struct {
	mu       sync.Mutex
	rows     []tunnel.State
	policy   tunnel.Policy
	toggled  []int
	attached map[int]bool
	schemes  map[int]tunnel.Scheme
}

func newStub(rows ...tunnel.State) *stubController {
	return &stubController{
		rows:     rows,
		policy:   tunnel.DefaultPolicy(),
		attached: map[int]bool{},
		schemes:  map[int]tunnel.Scheme{},
	}
}

func (s *stubController) CycleScheme(port int) tunnel.Scheme {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.schemes[port].Next()
	s.schemes[port] = next
	for i := range s.rows {
		if s.rows[i].RemotePort == port {
			s.rows[i].Scheme = next
		}
	}
	return next
}

func (s *stubController) States() []tunnel.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]tunnel.State(nil), s.rows...)
}

func (s *stubController) Toggle(port int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toggled = append(s.toggled, port)
	s.attached[port] = !s.attached[port]
	return s.attached[port]
}

func (s *stubController) Policy() tunnel.Policy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policy
}

func (s *stubController) SetPolicy(p tunnel.Policy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policy = p
}

var testNow = time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

// row builds an active tunnel row.
func row(remote, local int, cmd string) tunnel.State {
	return tunnel.State{
		RemotePort: remote,
		LocalPort:  local,
		LocalAddr:  "127.0.0.1",
		Cmd:        cmd,
		Proc:       strings.Fields(cmd + " ")[0],
		Status:     tunnel.StatusActive,
		Created:    testNow.Add(-5 * time.Minute),
		FirstSeen:  testNow.Add(-5 * time.Minute),
		Binds:      "127.0.0.1",
		PID:        1000 + remote,
	}
}

// skippedRow builds a non-forwarded row.
func skippedRow(remote int, cmd string, skip tunnel.Skip) tunnel.State {
	return tunnel.State{
		RemotePort:  remote,
		Cmd:         cmd,
		Status:      tunnel.StatusSkipped,
		Skip:        skip,
		Preexisting: skip == tunnel.SkipPreexising,
		FirstSeen:   testNow.Add(-time.Hour),
	}
}

// newModel builds a model with deterministic time and a fixed window size.
func newModel(t *testing.T, ctrl Controller) *Model {
	t.Helper()
	m := New(ctrl, Options{
		Host:     "devbox",
		Version:  "test",
		Dissolve: true,
		Now:      func() time.Time { return testNow },
		Rand:     rand.New(rand.NewSource(1)),
		OpenURL:  func(string) error { return nil },
	})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.reload()
	return m
}

// key builds a KeyMsg from the string form bubbletea reports.
func key(s string) tea.KeyMsg {
	switch s {
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// send delivers keys in order.
func send(m *Model, keys ...string) {
	for _, k := range keys {
		m.Update(key(k))
	}
}

func TestModelStartsOnTheFirstRow(t *testing.T) {
	m := newModel(t, newStub(row(3000, 3000, "node vite"), row(8080, 8080, "python3")))
	if m.cursor != 0 {
		t.Errorf("cursor = %d, want 0", m.cursor)
	}
	if got, _ := m.selected(); got.RemotePort != 3000 {
		t.Errorf("selected = %d, want 3000", got.RemotePort)
	}
}

func TestModelNavigation(t *testing.T) {
	m := newModel(t, newStub(row(3000, 3000, "a"), row(4000, 4000, "b"), row(5000, 5000, "c")))

	send(m, "j")
	if got, _ := m.selected(); got.RemotePort != 4000 {
		t.Errorf("after j, selected %d, want 4000", got.RemotePort)
	}
	send(m, "down")
	if got, _ := m.selected(); got.RemotePort != 5000 {
		t.Errorf("after down, selected %d, want 5000", got.RemotePort)
	}
	// The cursor must not run off the end.
	send(m, "j", "j", "j")
	if got, _ := m.selected(); got.RemotePort != 5000 {
		t.Errorf("cursor ran past the last row: %d", got.RemotePort)
	}
	send(m, "k", "up")
	if got, _ := m.selected(); got.RemotePort != 3000 {
		t.Errorf("after k/up, selected %d, want 3000", got.RemotePort)
	}
	send(m, "k", "k")
	if m.cursor != 0 {
		t.Errorf("cursor ran off the top: %d", m.cursor)
	}
	send(m, "G")
	if got, _ := m.selected(); got.RemotePort != 5000 {
		t.Errorf("G should jump to the last row, got %d", got.RemotePort)
	}
	send(m, "g")
	if m.cursor != 0 {
		t.Errorf("g should jump to the first row, got %d", m.cursor)
	}
}

func TestModelNavigationOnAnEmptyTable(t *testing.T) {
	m := newModel(t, newStub())
	send(m, "j", "k", "G", "g", "enter", "o", "y", "a")
	if m.cursor != 0 {
		t.Errorf("cursor = %d on an empty table, want 0", m.cursor)
	}
	if strings.Contains(m.View(), "panic") {
		t.Error("empty table should render cleanly")
	}
}

func TestModelQuitAsksFirst(t *testing.T) {
	m := newModel(t, newStub(row(3000, 3000, "node")))

	send(m, "esc")
	if !m.confirming {
		t.Fatal("esc should open the confirmation")
	}
	if !strings.Contains(m.View(), "quit?") {
		t.Error("the confirmation is not visible")
	}

	send(m, "n")
	if m.confirming {
		t.Error("n should dismiss the confirmation")
	}
	if m.quit {
		t.Error("n should not quit")
	}

	// q offers the same confirmation.
	send(m, "q")
	if !m.confirming {
		t.Error("q should open the confirmation")
	}
	send(m, "esc")
	if m.confirming {
		t.Error("esc should dismiss the confirmation")
	}
}

func TestModelConfirmStartsTheDissolve(t *testing.T) {
	m := newModel(t, newStub(row(3000, 3000, "node")))

	send(m, "esc", "y")
	if m.dissolving == nil {
		t.Fatal("confirming should start the dissolve animation")
	}
	if m.quit {
		t.Error("the model should not quit until the animation finishes")
	}

	// Drive the animation to completion.
	for i := 0; i < 1000 && !m.quit; i++ {
		m.Update(dissolveMsg(testNow))
	}
	if !m.quit {
		t.Error("the dissolve never completed")
	}
	if got := m.View(); got != "" {
		t.Errorf("the final view should be empty, got %q", got)
	}
}

func TestModelDissolveCanBeDisabled(t *testing.T) {
	m := New(newStub(row(3000, 3000, "node")), Options{
		Host: "devbox", Dissolve: false,
		Now: func() time.Time { return testNow },
	})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	send(m, "esc", "y")
	if m.dissolving != nil {
		t.Error("the animation should be skipped when disabled")
	}
	if !m.quit {
		t.Error("the model should quit immediately")
	}
}

// A key press during the animation skips straight to the end, because waiting
// out an animation you have already decided about is infuriating.
func TestModelKeySkipsTheDissolve(t *testing.T) {
	m := newModel(t, newStub(row(3000, 3000, "node")))
	send(m, "esc", "y")
	if m.dissolving == nil {
		t.Fatal("expected the animation to start")
	}
	send(m, "x")
	if !m.quit {
		t.Error("a key press should end the animation immediately")
	}
}

func TestModelCtrlCQuitsWithoutAsking(t *testing.T) {
	m := newModel(t, newStub(row(3000, 3000, "node")))
	send(m, "ctrl+c")
	if !m.quit {
		t.Error("ctrl+c should quit immediately")
	}
	if m.confirming {
		t.Error("ctrl+c should not open the confirmation")
	}
}

func TestModelSortCycles(t *testing.T) {
	m := newModel(t, newStub(row(3000, 3000, "a"), row(8080, 8080, "b")))

	if m.sortKey != SortRemote {
		t.Fatalf("initial sort = %q, want remote", m.sortKey)
	}
	seen := map[SortKey]bool{m.sortKey: true}
	for i := 0; i < len(sortKeys); i++ {
		send(m, "s")
		seen[m.sortKey] = true
	}
	if len(seen) != len(sortKeys) {
		t.Errorf("cycling visited %d keys, want all %d", len(seen), len(sortKeys))
	}
	if m.sortKey != SortRemote {
		t.Errorf("a full cycle should return to remote, got %q", m.sortKey)
	}

	send(m, "r")
	if !m.reverse {
		t.Error("r should reverse the sort")
	}
	send(m, "r")
	if m.reverse {
		t.Error("r should toggle")
	}
}

func TestModelFilter(t *testing.T) {
	m := newModel(t, newStub(
		row(3000, 3000, "node vite"),
		row(8080, 8080, "python3 -m http.server"),
		row(5432, 5432, "postgres"),
	))

	send(m, "/")
	if !m.filtering {
		t.Fatal("/ should open the filter")
	}
	send(m, "p", "y")
	if m.query != "py" {
		t.Errorf("query = %q, want py", m.query)
	}
	if len(m.rows) != 1 || m.rows[0].RemotePort != 8080 {
		t.Errorf("filtered rows = %+v, want only 8080", m.rows)
	}

	// Enter keeps the filter and closes the editor.
	send(m, "enter")
	if m.filtering {
		t.Error("enter should close the filter editor")
	}
	if m.query != "py" {
		t.Error("enter should keep the query")
	}

	// Esc from the table does not clear a filter; it opens the quit prompt.
	send(m, "/", "esc")
	if m.query != "" {
		t.Errorf("esc inside the filter should clear it, query = %q", m.query)
	}
	if len(m.rows) != 3 {
		t.Errorf("clearing the filter should restore all rows, got %d", len(m.rows))
	}
}

func TestModelFilterMatchesPortNumbers(t *testing.T) {
	m := newModel(t, newStub(row(3000, 3000, "node"), row(8080, 8080, "python3")))
	send(m, "/", "8", "0", "8")
	if len(m.rows) != 1 || m.rows[0].RemotePort != 8080 {
		t.Errorf("filtering by port gave %+v", m.rows)
	}
}

func TestModelToggleAttach(t *testing.T) {
	stub := newStub(skippedRow(5432, "postgres", tunnel.SkipPreexising))
	m := newModel(t, stub)

	send(m, "a")
	stub.mu.Lock()
	toggled := append([]int(nil), stub.toggled...)
	stub.mu.Unlock()

	if len(toggled) != 1 || toggled[0] != 5432 {
		t.Errorf("toggled = %v, want [5432]", toggled)
	}
	if !m.hasToast {
		t.Error("toggling should show feedback")
	}
	if !strings.Contains(m.toast.Text, "5432") {
		t.Errorf("toast = %q, should name the port", m.toast.Text)
	}
}

func TestModelPauseTogglesPolicy(t *testing.T) {
	stub := newStub(row(3000, 3000, "node"))
	m := newModel(t, stub)

	send(m, "p")
	if !stub.Policy().Paused {
		t.Error("p should pause automatic forwarding")
	}
	if !strings.Contains(m.View(), "PAUSED") {
		t.Error("the paused state should be visible in the footer")
	}

	send(m, "p")
	if stub.Policy().Paused {
		t.Error("p should un-pause")
	}
}

func TestModelToggleExisting(t *testing.T) {
	stub := newStub(skippedRow(5432, "postgres", tunnel.SkipPreexising))
	m := newModel(t, stub)

	send(m, "e")
	if !stub.Policy().Existing {
		t.Error("e should enable forwarding pre-existing ports")
	}
	send(m, "e")
	if stub.Policy().Existing {
		t.Error("e should toggle")
	}
}

func TestModelOpenURL(t *testing.T) {
	var opened []string
	stub := newStub(row(3000, 3000, "node"))
	m := New(stub, Options{
		Host: "devbox", Now: func() time.Time { return testNow },
		OpenURL: func(u string) error { opened = append(opened, u); return nil },
	})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.reload()

	send(m, "o")
	if len(opened) != 1 || opened[0] != "http://127.0.0.1:3000" {
		t.Errorf("opened = %v, want the tunnel URL", opened)
	}
}

func TestModelOpenSkippedRowReportsNoTunnel(t *testing.T) {
	m := newModel(t, newStub(skippedRow(5432, "postgres", tunnel.SkipPreexising)))
	send(m, "o")
	if !m.toast.Bad {
		t.Error("opening a row with no tunnel should report an error")
	}
}

func TestModelCopyEmitsOSC52(t *testing.T) {
	m := newModel(t, newStub(row(3000, 3000, "node")))
	send(m, "y")

	if m.pendingOSC == "" {
		t.Fatal("y should queue a clipboard sequence")
	}
	if m.pendingOSC != OSC52("http://127.0.0.1:3000") {
		t.Errorf("pendingOSC = %q", m.pendingOSC)
	}
	if !strings.HasPrefix(m.View(), m.pendingOSC) {
		t.Error("the sequence should be emitted with the frame")
	}

	// It must be sent exactly once.
	m.Update(refreshMsg(testNow))
	if m.pendingOSC != "" {
		t.Error("the sequence should be cleared after one frame")
	}
}

func TestModelDetailOverlay(t *testing.T) {
	m := newModel(t, newStub(row(3000, 3000, "node /app/server.js")))

	send(m, "enter")
	if !m.showDetail {
		t.Fatal("enter should open the detail pane")
	}
	view := m.View()
	for _, want := range []string{"remote :3000", "/app/server.js", "http://127.0.0.1:3000"} {
		if !strings.Contains(view, want) {
			t.Errorf("the detail pane is missing %q", want)
		}
	}

	send(m, "esc")
	if m.showDetail {
		t.Error("esc should close the detail pane")
	}
}

func TestModelHelpOverlay(t *testing.T) {
	m := newModel(t, newStub(row(3000, 3000, "node")))

	send(m, "?")
	if !m.showHelp {
		t.Fatal("? should open help")
	}
	if !strings.Contains(m.View(), "open in browser") {
		t.Error("the help text is missing")
	}

	send(m, "?")
	if m.showHelp {
		t.Error("? should close help")
	}
}

func TestModelStatusMessages(t *testing.T) {
	m := newModel(t, newStub(row(3000, 3000, "node")))

	m.Update(StatusMsg{State: Connected, Mode: "ss"})
	if !strings.Contains(m.View(), "connected (ss)") {
		t.Error("the connected status is not shown")
	}

	m.Update(StatusMsg{State: Reconnecting, Attempt: 3})
	if !strings.Contains(m.View(), "reconnecting #3") {
		t.Error("the reconnect attempt is not shown")
	}

	m.Update(StatusMsg{State: Disconnected})
	if !strings.Contains(m.View(), "disconnected") {
		t.Error("the disconnected status is not shown")
	}
}

func TestModelFatalMsg(t *testing.T) {
	m := newModel(t, newStub())
	want := errTest{}
	m.Update(FatalMsg{Err: want})
	if m.Err() != want {
		t.Errorf("Err() = %v, want the fatal error", m.Err())
	}
}

type errTest struct{}

func (errTest) Error() string { return "boom" }

func TestModelReloadPreservesSelection(t *testing.T) {
	stub := newStub(row(3000, 3000, "a"), row(8080, 8080, "b"))
	m := newModel(t, stub)

	send(m, "j") // select 8080

	// A new lower-numbered service appears and shifts the row order.
	stub.mu.Lock()
	stub.rows = []tunnel.State{row(1234, 1234, "c"), row(3000, 3000, "a"), row(8080, 8080, "b")}
	stub.mu.Unlock()
	m.Update(refreshMsg(testNow))

	if got, _ := m.selected(); got.RemotePort != 8080 {
		t.Errorf("selection moved to %d, want to stay on 8080", got.RemotePort)
	}
}

func TestModelToastExpires(t *testing.T) {
	m := newModel(t, newStub(row(3000, 3000, "node")))

	m.Update(ToastMsg{Text: "hello"})
	if !m.hasToast {
		t.Fatal("the toast was not shown")
	}
	m.Update(toastExpiredMsg(m.toastID))
	if m.hasToast {
		t.Error("the toast should expire")
	}

	// A stale expiry must not clear a newer toast.
	m.Update(ToastMsg{Text: "first"})
	stale := m.toastID
	m.Update(ToastMsg{Text: "second"})
	m.Update(toastExpiredMsg(stale))
	if !m.hasToast {
		t.Error("a stale expiry cleared the current toast")
	}
}

func TestModelScrollsWithASmallWindow(t *testing.T) {
	var rows []tunnel.State
	for i := 0; i < 50; i++ {
		rows = append(rows, row(3000+i, 3000+i, "svc"))
	}
	m := newModel(t, newStub(rows...))
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 12})

	send(m, "G")
	if m.offset == 0 {
		t.Error("the viewport should have scrolled to reach the last row")
	}
	if m.cursor < m.offset || m.cursor >= m.offset+m.tableHeight() {
		t.Errorf("cursor %d is outside the viewport [%d,%d)", m.cursor, m.offset, m.offset+m.tableHeight())
	}

	send(m, "g")
	if m.offset != 0 {
		t.Errorf("offset = %d after jumping to the top, want 0", m.offset)
	}
}

func TestModelInitReturnsATick(t *testing.T) {
	m := newModel(t, newStub())
	if cmd := m.Init(); cmd == nil {
		t.Error("Init should start the refresh ticker")
	}
}
