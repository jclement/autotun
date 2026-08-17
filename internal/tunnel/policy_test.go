package tunnel

import (
	"testing"

	"github.com/jclement/autotun/internal/probe"
)

func svc(port int, addrs ...string) probe.Service {
	s := probe.Service{Port: port, Proc: "node"}
	if len(addrs) == 0 {
		addrs = []string{"127.0.0.1"}
	}
	for _, a := range addrs {
		s.Binds = append(s.Binds, probe.Bind{Proto: "tcp", Addr: a})
	}
	return s
}

func mustSet(t *testing.T, spec string) PortSet {
	t.Helper()
	ps, err := ParsePortSet(spec)
	if err != nil {
		t.Fatalf("ParsePortSet(%q): %v", spec, err)
	}
	return ps
}

func TestPolicyDefaults(t *testing.T) {
	p := DefaultPolicy()
	tests := []struct {
		name        string
		svc         probe.Service
		preexisting bool
		want        Skip
	}{
		{"new high port is forwarded", svc(3000), false, SkipNone},
		{"pre-existing is skipped", svc(3000), true, SkipPreexising},
		{"privileged port is skipped", svc(80), false, SkipBelowMin},
		{"boundary 1024 is forwarded", svc(1024), false, SkipNone},
		{"boundary 1023 is skipped", svc(1023), false, SkipBelowMin},
		{"wildcard bind is forwarded by default", svc(3000, "0.0.0.0"), false, SkipNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.Eval(tt.svc, tt.preexisting); got != tt.want {
				t.Errorf("Eval() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPolicyExistingFlag(t *testing.T) {
	p := DefaultPolicy()
	p.Existing = true
	if got := p.Eval(svc(3000), true); got != SkipNone {
		t.Errorf("with --existing a pre-existing service should forward, got %q", got)
	}
}

func TestPolicyExclude(t *testing.T) {
	p := DefaultPolicy()
	p.Exclude = mustSet(t, "3000,9000-9100")

	for _, port := range []int{3000, 9000, 9050, 9100} {
		if got := p.Eval(svc(port), false); got != SkipExcluded {
			t.Errorf("port %d = %q, want excluded", port, got)
		}
	}
	if got := p.Eval(svc(3001), false); got != SkipNone {
		t.Errorf("port 3001 = %q, want forwarded", got)
	}
}

// --exclude has to beat --include, otherwise there is no way to punch a hole
// in a range.
func TestPolicyExcludeBeatsInclude(t *testing.T) {
	p := DefaultPolicy()
	p.Include = mustSet(t, "8000-9000")
	p.Exclude = mustSet(t, "8080")

	if got := p.Eval(svc(8080), false); got != SkipExcluded {
		t.Errorf("port 8080 = %q, want excluded", got)
	}
	if got := p.Eval(svc(8081), false); got != SkipNone {
		t.Errorf("port 8081 = %q, want forwarded", got)
	}
}

func TestPolicyIncludeIsExclusive(t *testing.T) {
	p := DefaultPolicy()
	p.Include = mustSet(t, "3000")

	if got := p.Eval(svc(3000), false); got != SkipNone {
		t.Errorf("port 3000 = %q, want forwarded", got)
	}
	if got := p.Eval(svc(4000), false); got != SkipNotInclude {
		t.Errorf("port 4000 = %q, want not-in-include", got)
	}
}

// Naming a port explicitly is an unambiguous request for it, so --include
// overrides the pre-existing rule and the port window.
func TestPolicyIncludeOverridesOtherRules(t *testing.T) {
	p := DefaultPolicy()
	p.Include = mustSet(t, "80,5432")

	if got := p.Eval(svc(80), false); got != SkipNone {
		t.Errorf("explicitly included port 80 = %q, want forwarded despite --min-port", got)
	}
	if got := p.Eval(svc(5432), true); got != SkipNone {
		t.Errorf("explicitly included pre-existing port = %q, want forwarded", got)
	}
}

func TestPolicyPortWindow(t *testing.T) {
	p := DefaultPolicy()
	p.MinPort, p.MaxPort = 3000, 4000

	if got := p.Eval(svc(2999), false); got != SkipBelowMin {
		t.Errorf("port 2999 = %q, want below-min", got)
	}
	if got := p.Eval(svc(4001), false); got != SkipAboveMax {
		t.Errorf("port 4001 = %q, want above-max", got)
	}
	if got := p.Eval(svc(3500), false); got != SkipNone {
		t.Errorf("port 3500 = %q, want forwarded", got)
	}
}

func TestPolicyRemoteBindLoopback(t *testing.T) {
	p := DefaultPolicy()
	p.RemoteBind = BindLoopback

	tests := []struct {
		name string
		svc  probe.Service
		want Skip
	}{
		{"loopback only", svc(3000, "127.0.0.1"), SkipNone},
		{"both loopbacks", svc(3000, "127.0.0.1", "::1"), SkipNone},
		{"wildcard", svc(3000, "0.0.0.0"), SkipNotLoop},
		{"mixed", svc(3000, "127.0.0.1", "0.0.0.0"), SkipNotLoop},
		{"specific address", svc(3000, "10.0.0.5"), SkipNotLoop},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.Eval(tt.svc, false); got != tt.want {
				t.Errorf("Eval() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPolicyPaused(t *testing.T) {
	p := DefaultPolicy()
	p.Paused = true
	if got := p.Eval(svc(3000), false); got != SkipPaused {
		t.Errorf("Eval() = %q, want paused", got)
	}
	// Pausing must not mask a more specific reason being wrong; excluded
	// still reports excluded.
	p.Exclude = mustSet(t, "3000")
	if got := p.Eval(svc(3000), false); got != SkipExcluded {
		t.Errorf("Eval() = %q, want excluded", got)
	}
}

func TestParseRemoteBind(t *testing.T) {
	if got, err := ParseRemoteBind("any"); err != nil || got != BindAny {
		t.Errorf(`ParseRemoteBind("any") = %q, %v`, got, err)
	}
	if got, err := ParseRemoteBind("loopback"); err != nil || got != BindLoopback {
		t.Errorf(`ParseRemoteBind("loopback") = %q, %v`, got, err)
	}
	if _, err := ParseRemoteBind("sideways"); err == nil {
		t.Error("want an error for an unknown bind mode")
	}
}
