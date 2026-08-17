package ui

import (
	"math/rand"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/jclement/autotun/internal/config"
	"github.com/jclement/autotun/internal/tunnel"
)

// plainView renders the model with styling stripped, for content assertions.
func plainView(m *Model) string { return ansi.Strip(m.View()) }

func TestViewRendersTheTable(t *testing.T) {
	m := newModel(t, newStub(
		row(3000, 3000, "node vite"),
		row(8080, 9090, "python3 -m http.server"),
	))
	view := plainView(m)

	for _, want := range []string{"autotun", "devbox", "LOCAL", "REMOTE", "PROCESS", "3000", "9090", "node vite"} {
		if !strings.Contains(view, want) {
			t.Errorf("the view is missing %q:\n%s", want, view)
		}
	}
}

// Nothing may exceed the terminal width, or the whole table wraps and shears.
func TestViewNeverExceedsTheTerminalWidth(t *testing.T) {
	rows := []tunnel.State{
		row(3000, 3000, "node /a/very/long/path/to/some/server.js --with --many --flags --indeed"),
		row(8080, 8080, strings.Repeat("x", 400)),
		skippedRow(5432, strings.Repeat("y", 200), tunnel.SkipPreexising),
	}
	for _, width := range []int{40, 60, 80, 100, 200} {
		m := newModel(t, newStub(rows...))
		m.Update(tea.WindowSizeMsg{Width: width, Height: 20})
		m.reload()

		for _, line := range strings.Split(m.View(), "\n") {
			if w := ansi.StringWidth(line); w > width {
				t.Errorf("at width %d a line is %d cells wide:\n%q", width, w, ansi.Strip(line))
			}
		}
	}
}

func TestViewRendersOverlaysWithinTheWindow(t *testing.T) {
	m := newModel(t, newStub(row(3000, 3000, "node")))
	for _, keys := range [][]string{{"?"}, {"enter"}, {"esc"}} {
		m := newModel(t, newStub(row(3000, 3000, "node")))
		send(m, keys...)
		for _, line := range strings.Split(m.View(), "\n") {
			if w := ansi.StringWidth(line); w > m.width {
				t.Errorf("overlay %v produced a %d-cell line, want at most %d", keys, w, m.width)
			}
		}
	}
	_ = m
}

func TestViewSkippedRowShowsTheReason(t *testing.T) {
	m := newModel(t, newStub(skippedRow(5432, "postgres", tunnel.SkipPreexising)))
	view := plainView(m)

	if !strings.Contains(view, "pre-existing") {
		t.Errorf("the skip reason is not shown:\n%s", view)
	}
	// A skipped row has no local port to show.
	if strings.Contains(view, "5432  ←") {
		t.Error("a skipped row should not render a forwarding arrow")
	}
}

func TestViewErrorRowShowsTheError(t *testing.T) {
	m := newModel(t, newStub(tunnel.State{
		RemotePort: 3000,
		Cmd:        "node",
		Status:     tunnel.StatusError,
		Err:        "local port 3000 is already in use",
		FirstSeen:  testNow,
	}))
	if !strings.Contains(plainView(m), "already in use") {
		t.Errorf("the error is not shown:\n%s", plainView(m))
	}
}

func TestViewMarksRemappedPorts(t *testing.T) {
	direct := row(3000, 3000, "node")
	remapped := row(8080, 41234, "python3")
	remapped.Remapped = true

	m := newModel(t, newStub(direct, remapped))
	view := plainView(m)

	if !strings.Contains(view, "←") {
		t.Error("a direct mapping should use ←")
	}
	if !strings.Contains(view, "≠") {
		t.Error("a remapped port should be flagged with ≠")
	}
}

func TestViewEmptyStates(t *testing.T) {
	m := newModel(t, newStub())
	m.Update(StatusMsg{State: Connected, Mode: "ss"})
	if !strings.Contains(plainView(m), "no services yet") {
		t.Errorf("connected-but-empty should explain itself:\n%s", plainView(m))
	}

	m.Update(StatusMsg{State: Reconnecting})
	if !strings.Contains(plainView(m), "waiting for the connection") {
		t.Errorf("disconnected-and-empty should explain itself:\n%s", plainView(m))
	}

	m2 := newModel(t, newStub(row(3000, 3000, "node")))
	send(m2, "/", "z", "z", "z")
	if !strings.Contains(plainView(m2), "nothing matches") {
		t.Errorf("an empty filter result should say so:\n%s", plainView(m2))
	}
}

func TestViewActivityMarkers(t *testing.T) {
	live := row(3000, 3000, "node")
	live.ActiveConns = 2

	recent := row(3001, 3001, "node")
	recent.ActiveConns = 0
	recent.LastByte = testNow.Add(-time.Second)

	fresh := row(4000, 4000, "vite")
	fresh.Created = testNow.Add(-3 * time.Second)

	idle := row(5000, 5000, "old")
	idle.Created = testNow.Add(-time.Hour)

	tests := []struct {
		name string
		row  tunnel.State
		want activity
	}{
		{"open connections", live, activityLive},
		{"recent traffic", recent, activityLive},
		{"just created", fresh, activityFresh},
		{"idle", idle, activityNone},
		{"skipped is never highlighted", skippedRow(5432, "pg", tunnel.SkipPreexising), activityNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := activityOf(tt.row, testNow); got != tt.want {
				t.Errorf("activityOf() = %v, want %v", got, tt.want)
			}
		})
	}

	m := newModel(t, newStub(live, fresh, idle))
	view := plainView(m)
	if !strings.Contains(view, "●") {
		t.Error("an in-use tunnel should carry the live marker")
	}
	if !strings.Contains(view, "◦") {
		t.Error("a brand new tunnel should carry the fresh marker")
	}
}

func TestViewSchemeColumn(t *testing.T) {
	https := row(3000, 3000, "node")
	https.Scheme = tunnel.SchemeHTTPS
	unknown := row(5432, 5432, "postgres")

	m := newModel(t, newStub(https, unknown))
	view := plainView(m)

	if !strings.Contains(view, "VIA") {
		t.Error("the scheme column header is missing")
	}
	if !strings.Contains(view, "https") {
		t.Error("a detected https port should be labeled")
	}
	if !strings.Contains(view, "unknown") {
		t.Errorf("an undetermined scheme should say so plainly:\n%s", view)
	}
}

// Every row carries a VIA value, including ones that are not forwarded, so the
// column is never a block of blanks.
func TestViewSchemeShownOnSkippedRows(t *testing.T) {
	m := newModel(t, newStub(skippedRow(5432, "postgres", tunnel.SkipPreexising)))
	if !strings.Contains(plainView(m), "unknown") {
		t.Errorf("a skipped row should still show its protocol:\n%s", plainView(m))
	}
}

func TestViewHeaderCounts(t *testing.T) {
	m := newModel(t, newStub(
		row(3000, 3000, "a"),
		row(4000, 4000, "b"),
		skippedRow(5432, "pg", tunnel.SkipPreexising),
	))
	view := plainView(m)

	if !strings.Contains(view, "2 tunnels") {
		t.Errorf("the active count is wrong:\n%s", view)
	}
	if !strings.Contains(view, "1 idle") {
		t.Errorf("the idle count is wrong:\n%s", view)
	}
}

func TestViewSingularTunnelCount(t *testing.T) {
	m := newModel(t, newStub(row(3000, 3000, "a")))
	if !strings.Contains(plainView(m), "1 tunnel ") {
		t.Errorf("a single tunnel should not be pluralized:\n%s", plainView(m))
	}
}

func TestOverlayCenterPreservesWidth(t *testing.T) {
	base := strings.Repeat("abcdefghij", 8) // 80 cells
	baseView := strings.Join([]string{base, base, base, base, base}, "\n")
	box := "+----+\n|  x |\n+----+"

	got := overlayCenter(baseView, box, 80, 5)
	for i, line := range strings.Split(got, "\n") {
		if w := ansi.StringWidth(line); w != 80 {
			t.Errorf("line %d is %d cells wide, want 80: %q", i, w, ansi.Strip(line))
		}
	}
	if !strings.Contains(ansi.Strip(got), "|  x |") {
		t.Error("the box was not composited in")
	}
}

func TestOverlayCenterHandlesAnOversizedBox(t *testing.T) {
	// Must not panic or index out of range when the box is bigger than the
	// window.
	got := overlayCenter("short", strings.Repeat("wide box\n", 10), 10, 3)
	if got == "" {
		t.Error("overlayCenter produced nothing")
	}
}

func TestOverlayCenterWithNoBox(t *testing.T) {
	if got := overlayCenter("base", "", 10, 3); got != "base" {
		t.Errorf("an empty box should leave the base alone, got %q", got)
	}
}

func TestViewSurvivesATinyWindow(t *testing.T) {
	m := newModel(t, newStub(row(3000, 3000, "node")))
	for _, size := range [][2]int{{1, 1}, {10, 3}, {20, 5}, {5, 40}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m.reload()
		if view := m.View(); view == "" && size[0] > 1 {
			t.Errorf("no output at %dx%d", size[0], size[1])
		}
		send(m, "?") // an overlay in a tiny window must not panic either
		_ = m.View()
		send(m, "?")
	}
}

func TestDefaultThemeIsComplete(t *testing.T) {
	th := DefaultTheme()
	// A zero Style renders its input unchanged; catching an unset style here
	// is cheaper than noticing an invisible column later.
	if th.Title.Render("x") == "" || th.Row.Render("x") == "" || th.Live.Render("x") == "" {
		t.Error("a theme style is unset")
	}
}

func TestDissolveTerminatesAndKeepsWidth(t *testing.T) {
	view := strings.Join([]string{
		strings.Repeat("a", 40),
		strings.Repeat("b", 40),
		strings.Repeat("c", 40),
	}, "\n")

	d := newDissolve(view, 40, 3, DefaultTheme(), rand.New(rand.NewSource(7)))
	frames := 0
	for !d.Done() {
		out := d.View()
		for _, line := range strings.Split(out, "\n") {
			if w := ansi.StringWidth(line); w > 40 {
				t.Fatalf("dissolve frame %d is %d cells wide, want at most 40", frames, w)
			}
		}
		d.Advance()
		if frames++; frames > 5000 {
			t.Fatal("the dissolve never finished")
		}
	}
	if frames < 5 {
		t.Errorf("the animation ran for only %d frames; it should be visible", frames)
	}
}

func TestDissolveStartsFromTheCapturedFrame(t *testing.T) {
	d := newDissolve("hello world", 11, 1, DefaultTheme(), rand.New(rand.NewSource(1)))
	if got := ansi.Strip(d.View()); got != "hello world" {
		t.Errorf("the first frame = %q, want the captured content", got)
	}
}

func TestDissolveEndsBlank(t *testing.T) {
	d := newDissolve("hello world", 11, 1, DefaultTheme(), rand.New(rand.NewSource(1)))
	for !d.Done() {
		d.Advance()
	}
	if got := strings.TrimSpace(ansi.Strip(d.View())); got != "" {
		t.Errorf("the final frame should be blank, got %q", got)
	}
}

func TestDissolveProgress(t *testing.T) {
	d := newDissolve("abc", 3, 1, DefaultTheme(), rand.New(rand.NewSource(1)))
	if p := d.Progress(); p != 0 {
		t.Errorf("initial progress = %v, want 0", p)
	}
	for !d.Done() {
		d.Advance()
	}
	if p := d.Progress(); p < 1 {
		t.Errorf("final progress = %v, want at least 1", p)
	}
}

func TestDissolveUsesSingleWidthGlyphs(t *testing.T) {
	// Full-width katakana would double every replaced cell and shear the frame.
	for _, r := range matrixRunes {
		if w := ansi.StringWidth(string(r)); w != 1 {
			t.Errorf("matrix rune %q is %d cells wide, want 1", string(r), w)
		}
	}
}

// The frame must be a closed box at every size, or the border characters
// scatter through the table.
func TestViewDrawsAClosedFrame(t *testing.T) {
	rows := []tunnel.State{
		row(3000, 3000, "node vite"),
		skippedRow(5432, "postgres", tunnel.SkipPreexising),
	}
	for _, size := range [][2]int{{40, 10}, {80, 24}, {120, 30}, {200, 12}} {
		m := newModel(t, newStub(rows...))
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m.reload()

		lines := strings.Split(m.View(), "\n")
		if len(lines) != size[1] {
			t.Errorf("at %dx%d the view is %d lines, want %d", size[0], size[1], len(lines), size[1])
		}
		for i, line := range lines {
			plain := []rune(ansi.Strip(line))
			if len(plain) == 0 {
				t.Errorf("at %dx%d line %d is empty", size[0], size[1], i)
				continue
			}
			first, last := string(plain[0]), string(plain[len(plain)-1])
			var wantFirst, wantLast string
			switch i {
			case 0:
				wantFirst, wantLast = cornerTL, cornerTR
			case len(lines) - 1:
				wantFirst, wantLast = cornerBL, cornerBR
			case rowSeparatorLine:
				wantFirst, wantLast = teeL, teeR
			default:
				wantFirst, wantLast = edgeV, edgeV
			}
			if first != wantFirst || last != wantLast {
				t.Errorf("at %dx%d line %d starts %q ends %q, want %q/%q",
					size[0], size[1], i, first, last, wantFirst, wantLast)
			}
		}
	}
}

// rowSeparatorLine is the rule between the column header and the data.
const rowSeparatorLine = 2

func TestViewShowsTheModeColumn(t *testing.T) {
	on := row(3000, 3000, "node")
	on.Mode = config.ModeOn
	off := skippedRow(5432, "postgres", tunnel.SkipOff)
	off.Mode = config.ModeOff

	m := newModel(t, newStub(on, off))
	view := plainView(m)

	if !strings.Contains(view, " M ") {
		t.Errorf("the mode column header is missing:\n%s", view)
	}
	if !strings.Contains(view, "+") {
		t.Error("an always-on port should be marked")
	}
	if !strings.Contains(view, "-") {
		t.Error("a never-forward port should be marked")
	}
}

func TestViewSearchAndViewChips(t *testing.T) {
	stub := newStub(row(3000, 3000, "node"))
	m := newModel(t, stub)

	send(m, "/", "n", "o", "d", "e", "enter")
	if !strings.Contains(plainView(m), "/node") {
		t.Errorf("an active search should be shown:\n%s", plainView(m))
	}

	// Listing pre-existing services is a mode worth showing in the footer.
	stub.prefs = config.DefaultViewPrefs()
	m2 := newModel(t, stub)
	send(m2, "c", " ")
	m2.menu.open = false
	if !strings.Contains(plainView(m2), "+pre-existing") {
		t.Errorf("the active view should be shown:\n%s", plainView(m2))
	}
}

func TestViewEditorAppearsInTheBottomBorder(t *testing.T) {
	m := newModel(t, newStub(row(3000, 3000, "node")))
	send(m, "down", "l")

	view := plainView(m)
	if !strings.Contains(view, "local port for remote 3000") {
		t.Errorf("the local port editor is not shown:\n%s", view)
	}
	if !strings.Contains(view, "blank resets") {
		t.Error("the editor should explain how to reset")
	}
}

// On a very wide terminal the table must stop stretching, or the right-hand
// columns end up a screenful away from the row they describe.
func TestViewCapsTheProcessColumn(t *testing.T) {
	m := newModel(t, newStub(row(3000, 3000, "node vite")))
	m.Update(tea.WindowSizeMsg{Width: 400, Height: 20})
	m.reload()

	if got := m.procWidth(); got != maxProc {
		t.Errorf("procWidth at 400 columns = %d, want it capped at %d", got, maxProc)
	}

	// The columns still line up under their headers.
	cols := m.columns()
	last := cols[len(cols)-1]
	if last.x+last.w > m.inner()+1 {
		t.Errorf("the last column ends at %d, past the frame", last.x+last.w)
	}

	// And a narrow terminal still gets a usable process column.
	m.Update(tea.WindowSizeMsg{Width: 50, Height: 20})
	if got := m.procWidth(); got != minProc {
		t.Errorf("procWidth at 50 columns = %d, want the %d floor", got, minProc)
	}
}

// The gap between a working connection and the first scan should say what is
// happening rather than still claiming to be connecting.
func TestViewShowsTheProbingStatus(t *testing.T) {
	m := newModel(t, newStub())
	m.Update(StatusMsg{State: Probing})
	if !strings.Contains(plainView(m), "starting remote prober") {
		t.Errorf("the probing status is not shown:\n%s", plainView(m))
	}
}

// The reason a row is not forwarded belongs beside the row, not stranded at the
// far right of a wide terminal: it starts where AGE — the first column it
// replaces — begins.
func TestViewSkipReasonStartsAtTheAgeColumn(t *testing.T) {
	m := newModel(t, newStub(skippedRow(5432, "postgres", tunnel.SkipPreexising)))
	m.Update(tea.WindowSizeMsg{Width: 200, Height: 12})
	m.reload()

	var ageX int
	for _, c := range m.columns() {
		if c.title == "AGE" {
			ageX = c.x
		}
	}
	if ageX == 0 {
		t.Fatal("no AGE column")
	}

	for _, line := range strings.Split(plainView(m), "\n") {
		byteIdx := strings.Index(line, "pre-existing")
		if byteIdx < 0 {
			continue
		}
		// The row contains multi-byte glyphs, so the byte offset is not the
		// display column; measure the width of what precedes the match.
		col := ansi.StringWidth(line[:byteIdx])
		if col != ageX {
			t.Errorf("the reason starts at column %d, want the AGE column at %d\n%s", col, ageX, line)
		}
		return
	}
	t.Error("the reason was not rendered")
}
