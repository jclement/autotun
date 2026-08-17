package ui

import (
	"testing"
	"time"

	"github.com/jclement/autotun/internal/tunnel"
)

// ports extracts the remote ports in order.
func ports(rows []tunnel.State) []int {
	out := make([]int, len(rows))
	for i, r := range rows {
		out[i] = r.RemotePort
	}
	return out
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sortFixture builds rows that differ on every sortable dimension.
func sortFixture() []tunnel.State {
	a := row(8080, 100, "zebra")
	a.FirstSeen = testNow.Add(-time.Hour)
	a.BytesIn, a.BytesOut = 1, 1
	a.TotalConns = 1

	b := row(3000, 300, "alpha")
	b.FirstSeen = testNow.Add(-time.Minute)
	b.BytesIn, b.BytesOut = 500, 500
	b.TotalConns = 9

	c := row(5000, 200, "middle")
	c.FirstSeen = testNow.Add(-10 * time.Minute)
	c.BytesIn, c.BytesOut = 50, 50
	c.TotalConns = 5

	return []tunnel.State{a, b, c}
}

func TestSortStates(t *testing.T) {
	tests := []struct {
		key  SortKey
		want []int
	}{
		{SortRemote, []int{3000, 5000, 8080}},
		{SortLocal, []int{8080, 5000, 3000}},   // local ports 100, 200, 300
		{SortProcess, []int{3000, 5000, 8080}}, // alpha, middle, zebra
		{SortAge, []int{3000, 5000, 8080}},     // newest first
		{SortTraffic, []int{3000, 5000, 8080}}, // busiest first
		{SortConns, []int{3000, 5000, 8080}},   // most connections first
	}
	for _, tt := range tests {
		t.Run(tt.key.String(), func(t *testing.T) {
			rows := sortFixture()
			sortStates(rows, tt.key, false, false)
			if got := ports(rows); !equal(got, tt.want) {
				t.Errorf("sort by %s = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

func TestSortStatesReversed(t *testing.T) {
	rows := sortFixture()
	sortStates(rows, SortRemote, true, false)
	if got := ports(rows); !equal(got, []int{8080, 5000, 3000}) {
		t.Errorf("reversed remote sort = %v", got)
	}
}

// Equal values must fall back to the remote port, so the table never jitters
// between refreshes.
func TestSortStatesIsStableOnTies(t *testing.T) {
	var rows []tunnel.State
	for _, p := range []int{9000, 3000, 5000} {
		r := row(p, p, "same")
		r.FirstSeen = testNow
		rows = append(rows, r)
	}
	for _, key := range sortKeys {
		got := append([]tunnel.State(nil), rows...)
		sortStates(got, key, false, false)
		if p := ports(got); !equal(p, []int{3000, 5000, 9000}) {
			t.Errorf("sort by %s broke the tie-break order: %v", key, p)
		}
	}
}

func TestSortKeyCycle(t *testing.T) {
	k := SortRemote
	seen := map[SortKey]bool{}
	for i := 0; i < len(sortKeys); i++ {
		if seen[k] {
			t.Fatalf("the cycle repeated %q early", k)
		}
		seen[k] = true
		k = k.Next()
	}
	if k != SortRemote {
		t.Errorf("the cycle did not return to the start, ended at %q", k)
	}
	// An out-of-range key must not wedge the cycle.
	if got := SortKey(99).Next(); got != SortRemote {
		t.Errorf("SortKey(99).Next() = %q, want remote", got)
	}
}

func TestFilterStates(t *testing.T) {
	rows := []tunnel.State{
		row(3000, 3000, "node vite"),
		row(8080, 8080, "python3 -m http.server"),
		skippedRow(5432, "postgres", tunnel.SkipPreexising),
	}
	tests := []struct {
		query string
		want  []int
	}{
		{"", []int{3000, 8080, 5432}},
		{"node", []int{3000}},
		{"NODE", []int{3000}}, // case-insensitive
		{"python", []int{8080}},
		{"3000", []int{3000}},
		{"80", []int{8080}},
		{"skipped", []int{5432}}, // status is searchable
		{"  node  ", []int{3000}},
		{"nothing", nil},
	}
	for _, tt := range tests {
		got := ports(filterStates(rows, tt.query))
		if !equal(got, tt.want) {
			t.Errorf("filter %q = %v, want %v", tt.query, got, tt.want)
		}
	}
}

// Filtering must not scribble over the caller's slice.
func TestFilterStatesDoesNotAliasTheInput(t *testing.T) {
	rows := []tunnel.State{
		row(3000, 3000, "node"),
		row(8080, 8080, "python3"),
	}
	filtered := filterStates(rows, "python")
	if len(filtered) != 1 {
		t.Fatalf("filter returned %d rows", len(filtered))
	}
	if rows[0].RemotePort != 3000 || rows[1].RemotePort != 8080 {
		t.Errorf("the input slice was modified: %v", ports(rows))
	}
}

// Inactive rows sink below live ones without disturbing the order within each
// group, so the tunnels you are using stay together at the top.
func TestSortInactiveLast(t *testing.T) {
	live := row(9000, 9000, "live")
	idle := skippedRow(3000, "idle", tunnel.SkipPreexising)
	live2 := row(5000, 5000, "live2")

	rows := []tunnel.State{idle, live, live2}
	sortStates(rows, SortRemote, false, true)

	if got := ports(rows); !equal(got, []int{5000, 9000, 3000}) {
		t.Errorf("order = %v, want the live rows first, still sorted by port", got)
	}

	// With grouping off it is a plain port sort.
	rows = []tunnel.State{idle, live, live2}
	sortStates(rows, SortRemote, false, false)
	if got := ports(rows); !equal(got, []int{3000, 5000, 9000}) {
		t.Errorf("order = %v, want a plain port sort", got)
	}
}

// Pre-existing services are the host's own furniture; they are hidden unless
// asked for, and a forwarded one is never hidden.
func TestHidePreexisting(t *testing.T) {
	forwarded := row(3000, 3000, "node")
	forwarded.Skip = tunnel.SkipPreexising // attached by hand, so still active

	rows := []tunnel.State{
		row(8080, 8080, "vite"),
		skippedRow(5432, "postgres", tunnel.SkipPreexising),
		forwarded,
	}
	got := ports(hidePreexisting(rows))
	if !equal(got, []int{8080, 3000}) {
		t.Errorf("visible ports = %v, want the idle pre-existing one hidden", got)
	}
}

func TestSortKeyNamed(t *testing.T) {
	for _, k := range sortKeys {
		if got := sortKeyNamed(k.String()); got != k {
			t.Errorf("sortKeyNamed(%q) = %q, want %q", k.String(), got, k)
		}
	}
	if got := sortKeyNamed("nonsense"); got != SortRemote {
		t.Errorf("an unknown name = %q, want the default", got)
	}
}
