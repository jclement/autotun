package ui

import (
	"math/rand"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
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

func TestFitLine(t *testing.T) {
	got := fitLine("left", "right", 20)
	if ansi.StringWidth(got) != 20 {
		t.Errorf("fitLine width = %d, want 20: %q", ansi.StringWidth(got), got)
	}
	if !strings.HasPrefix(got, "left") || !strings.Contains(got, "right") {
		t.Errorf("fitLine = %q", got)
	}

	// With no room, the right-hand side is dropped rather than wrapping.
	if got := fitLine("aaaaaaaa", "bbbbbbbb", 10); got != "aaaaaaaa" {
		t.Errorf("fitLine = %q, want the left side alone", got)
	}
	if got := fitLine("left", "", 20); got != "left" {
		t.Errorf("fitLine with no right side = %q", got)
	}
	if got := fitLine("left", "right", 0); got != "left" {
		t.Errorf("fitLine with no width = %q", got)
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
