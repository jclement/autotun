package app

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/jclement/autotun/internal/tunnel"
	"github.com/jclement/autotun/internal/ui"
)

// Renderer receives everything worth telling the user about, in the modes that
// do not run the TUI.
type Renderer interface {
	Event(tunnel.Event)
	Status(state ui.ConnState, detail string)
}

// LogRenderer writes human-readable lines, one per change.
type LogRenderer struct {
	mu  sync.Mutex
	w   io.Writer
	now func() time.Time
}

// NewLogRenderer writes to w.
func NewLogRenderer(w io.Writer) *LogRenderer {
	return &LogRenderer{w: w, now: time.Now}
}

func (r *LogRenderer) line(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(r.w, "%s  %s\n", r.now().Format("15:04:05"), msg)
}

// Event prints one tunnel change.
func (r *LogRenderer) Event(e tunnel.Event) { r.line(e.String()) }

// Status prints a connection transition.
func (r *LogRenderer) Status(state ui.ConnState, detail string) {
	msg := string(state)
	if detail != "" {
		msg += ": " + detail
	}
	r.line(msg)
}

// JSONRenderer writes newline-delimited JSON, one object per change.
type JSONRenderer struct {
	mu  sync.Mutex
	enc *json.Encoder
	now func() time.Time
}

// NewJSONRenderer writes to w.
func NewJSONRenderer(w io.Writer) *JSONRenderer {
	return &JSONRenderer{enc: json.NewEncoder(w), now: time.Now}
}

// tunnelEvent is the wire shape of a forwarding change.
type tunnelEvent struct {
	Type       string `json:"type"`
	Time       string `json:"time"`
	Event      string `json:"event"`
	RemotePort int    `json:"remote_port"`
	LocalAddr  string `json:"local_addr,omitempty"`
	LocalPort  int    `json:"local_port,omitempty"`
	Remapped   bool   `json:"remapped,omitempty"`
	PID        int    `json:"pid,omitempty"`
	Command    string `json:"command,omitempty"`
	URL        string `json:"url,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// statusEvent is the wire shape of a connection transition.
type statusEvent struct {
	Type   string `json:"type"`
	Time   string `json:"time"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// Event writes one tunnel change.
func (r *JSONRenderer) Event(e tunnel.Event) {
	s := e.State
	r.write(tunnelEvent{
		Type:       "tunnel",
		Time:       r.now().UTC().Format(time.RFC3339),
		Event:      string(e.Kind),
		RemotePort: s.RemotePort,
		LocalAddr:  s.LocalAddr,
		LocalPort:  s.LocalPort,
		Remapped:   s.Remapped,
		PID:        s.PID,
		Command:    s.Cmd,
		URL:        s.URL(),
		Reason:     e.Msg,
	})
}

// Status writes a connection transition.
func (r *JSONRenderer) Status(state ui.ConnState, detail string) {
	r.write(statusEvent{
		Type:   "status",
		Time:   r.now().UTC().Format(time.RFC3339),
		State:  string(state),
		Detail: detail,
	})
}

func (r *JSONRenderer) write(v any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.enc.Encode(v)
}

// nopRenderer discards everything; used when the TUI owns the screen.
type nopRenderer struct{}

func (nopRenderer) Event(tunnel.Event)          {}
func (nopRenderer) Status(ui.ConnState, string) {}
