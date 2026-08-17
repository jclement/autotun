package app

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jclement/autotun/internal/tunnel"
	"github.com/jclement/autotun/internal/ui"
)

var renderNow = time.Date(2026, 8, 17, 14, 30, 5, 0, time.UTC)

func openedEvent() tunnel.Event {
	return tunnel.Event{
		Kind: tunnel.EventOpened,
		State: tunnel.State{
			RemotePort: 3000,
			LocalPort:  3000,
			LocalAddr:  "127.0.0.1",
			PID:        4242,
			Cmd:        "node /app/server.js",
			Status:     tunnel.StatusActive,
			Scheme:     tunnel.SchemeHTTPS,
		},
	}
}

func TestLogRenderer(t *testing.T) {
	var buf strings.Builder
	r := NewLogRenderer(&buf)
	r.now = func() time.Time { return renderNow }

	r.Status(ui.Connected, "")
	r.Event(openedEvent())
	r.Status(ui.Disconnected, "connection reset")

	out := buf.String()
	for _, want := range []string{"14:30:05", "connected", "opened", "3000", "node /app/server.js", "connection reset"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log is missing %q:\n%s", want, out)
		}
	}
	if n := strings.Count(strings.TrimSpace(out), "\n"); n != 2 {
		t.Errorf("got %d newlines, want one line per event:\n%s", n, out)
	}
}

func TestJSONRenderer(t *testing.T) {
	var buf strings.Builder
	r := NewJSONRenderer(&buf)
	r.now = func() time.Time { return renderNow }

	r.Event(openedEvent())
	r.Status(ui.Disconnected, "connection reset")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), buf.String())
	}

	var ev map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatalf("line 1 is not JSON: %v", err)
	}
	if ev["type"] != "tunnel" || ev["event"] != "opened" {
		t.Errorf("tunnel event = %v", ev)
	}
	if ev["remote_port"] != float64(3000) || ev["local_port"] != float64(3000) {
		t.Errorf("ports = %v / %v", ev["remote_port"], ev["local_port"])
	}
	if ev["url"] != "https://127.0.0.1:3000" {
		t.Errorf("url = %v, want the scheme to be honored", ev["url"])
	}
	if ev["time"] != "2026-08-17T14:30:05Z" {
		t.Errorf("time = %v, want RFC 3339 UTC", ev["time"])
	}

	var st map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &st); err != nil {
		t.Fatalf("line 2 is not JSON: %v", err)
	}
	if st["type"] != "status" || st["state"] != "disconnected" || st["detail"] != "connection reset" {
		t.Errorf("status event = %v", st)
	}
}

// Empty optional fields must be omitted so the stream stays easy to filter.
func TestJSONRendererOmitsEmptyFields(t *testing.T) {
	var buf strings.Builder
	r := NewJSONRenderer(&buf)
	r.now = func() time.Time { return renderNow }

	r.Event(tunnel.Event{Kind: tunnel.EventFailed, State: tunnel.State{RemotePort: 3000}, Msg: "busy"})

	var ev map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &ev); err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"local_port", "pid", "command", "url", "remapped"} {
		if _, ok := ev[absent]; ok {
			t.Errorf("field %q should be omitted when empty: %v", absent, ev)
		}
	}
	if ev["reason"] != "busy" {
		t.Errorf("reason = %v", ev["reason"])
	}
}

func TestNopRendererIsSilent(t *testing.T) {
	var r Renderer = nopRenderer{}
	r.Event(openedEvent())
	r.Status(ui.Connected, "")
}

func TestRenderersAreConcurrencySafe(t *testing.T) {
	var buf syncBuffer
	for _, r := range []Renderer{NewLogRenderer(&buf), NewJSONRenderer(&buf)} {
		done := make(chan struct{})
		for i := 0; i < 4; i++ {
			go func() {
				defer func() { done <- struct{}{} }()
				for j := 0; j < 50; j++ {
					r.Event(openedEvent())
					r.Status(ui.Connected, "")
				}
			}()
		}
		for i := 0; i < 4; i++ {
			<-done
		}
	}
}

// syncBuffer is a minimal concurrency-safe writer for the race check above.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
