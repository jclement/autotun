package ui

import (
	"fmt"
	"testing"
	"time"

	"github.com/jclement/autotun/internal/tunnel"
)

// TestScreenshot renders a representative table so the layout can be eyeballed:
//
//	go test ./internal/ui -run Screenshot -v
//
// It also asserts the things that are easy to break silently, so it earns its
// place beyond being a viewer.
func TestScreenshot(t *testing.T) {
	live := row(3000, 3000, "node /usr/local/bin/vite --port 3000")
	live.ActiveConns = 2
	live.BytesIn, live.BytesOut = 1258291, 348160
	live.Scheme = tunnel.SchemeHTTP
	live.Created = testNow.Add(-14 * time.Minute)

	remapped := row(5173, 41733, "node vite --host 0.0.0.0")
	remapped.Remapped = true
	remapped.Scheme = tunnel.SchemeHTTPS
	remapped.SchemePinned = true
	remapped.Created = testNow.Add(-9 * time.Minute)

	fresh := row(8080, 8080, "python3 -m http.server 8080")
	// No connections yet and just created: this is the "fresh" highlight.
	fresh.Created = testNow.Add(-4 * time.Second)

	failed := tunnel.State{
		RemotePort: 9229,
		Cmd:        "node --inspect",
		Status:     tunnel.StatusError,
		Err:        "local port 9229 is already in use",
		FirstSeen:  testNow.Add(-time.Minute),
	}

	rows := []tunnel.State{
		live,
		remapped,
		fresh,
		failed,
		skippedRow(5432, "postgres -D /var/lib/postgresql/data", tunnel.SkipPreexising),
		skippedRow(6379, "redis-server *:6379", tunnel.SkipPreexising),
	}

	m := newModel(t, newStub(rows...))
	m.Update(StatusMsg{State: Connected, Mode: "ss"})
	m.reload()

	view := m.View()
	fmt.Printf("\n%s\n\n", view)

	// The layout claims are worth holding onto.
	for _, want := range []string{"devbox", "connected (ss)", "3 tunnels", "≠", "←", "●", "◦", "https", "already in use"} {
		if !contains(view, want) {
			t.Errorf("the rendered table is missing %q", want)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
