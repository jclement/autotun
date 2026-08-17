package tunnel

import (
	"fmt"

	"github.com/jclement/autotun/internal/probe"
)

// RemoteBind selects which remote bind addresses are interesting.
type RemoteBind string

const (
	// BindAny forwards a service regardless of what it is bound to.
	BindAny RemoteBind = "any"
	// BindLoopback forwards only services bound exclusively to loopback:
	// exactly the ones no firewall change could make reachable.
	BindLoopback RemoteBind = "loopback"
)

// ParseRemoteBind validates a --remote-bind value.
func ParseRemoteBind(s string) (RemoteBind, error) {
	switch RemoteBind(s) {
	case BindAny:
		return BindAny, nil
	case BindLoopback:
		return BindLoopback, nil
	}
	return "", fmt.Errorf("invalid --remote-bind %q (want \"any\" or \"loopback\")", s)
}

// Policy decides which remote services autotun forwards.
type Policy struct {
	MinPort    int
	MaxPort    int
	Include    PortSet
	Exclude    PortSet
	RemoteBind RemoteBind
	// Existing forwards services that were already listening when autotun
	// connected. Off by default so a dev box's sshd, database and container
	// runtime do not get scooped up.
	Existing bool
	// Paused suspends automatic forwarding without discarding state.
	Paused bool
}

// DefaultPolicy is the policy applied when no flags are given.
func DefaultPolicy() Policy {
	return Policy{
		MinPort:    1024,
		MaxPort:    65535,
		RemoteBind: BindAny,
	}
}

// Skip explains why a service is not forwarded. An empty Skip means "forward".
type Skip string

const (
	SkipNone       Skip = ""
	SkipPreexising Skip = "pre-existing"
	SkipBelowMin   Skip = "below --min-port"
	SkipAboveMax   Skip = "above --max-port"
	SkipExcluded   Skip = "excluded"
	SkipNotInclude Skip = "not in --include"
	SkipNotLoop    Skip = "not loopback-only"
	SkipPaused     Skip = "paused"
)

// Eval decides whether svc should be forwarded. preexisting reports whether the
// service was present in the baseline scan taken at connect time.
//
// An explicit --include entry overrides the min/max window and the pre-existing
// rule: naming a port is an unambiguous request for it.
func (p Policy) Eval(svc probe.Service, preexisting bool) Skip {
	if p.Exclude.Contains(svc.Port) {
		return SkipExcluded
	}
	explicit := p.Include.Contains(svc.Port)
	if !explicit {
		if !p.Include.Empty() {
			return SkipNotInclude
		}
		if svc.Port < p.MinPort {
			return SkipBelowMin
		}
		if svc.Port > p.MaxPort {
			return SkipAboveMax
		}
		if p.RemoteBind == BindLoopback && !svc.LoopbackOnly() {
			return SkipNotLoop
		}
		if preexisting && !p.Existing {
			return SkipPreexising
		}
	}
	if p.Paused {
		return SkipPaused
	}
	return SkipNone
}
