package ui

import (
	"sort"
	"strconv"
	"strings"

	"github.com/jclement/autotun/internal/tunnel"
)

// SortKey selects the table's ordering.
type SortKey int

const (
	SortRemote SortKey = iota
	SortLocal
	SortProcess
	SortAge
	SortTraffic
	SortConns
)

// sortKeys is the cycle order for the `s` key.
var sortKeys = []SortKey{SortRemote, SortLocal, SortProcess, SortAge, SortTraffic, SortConns}

// String names the key for the status bar.
func (s SortKey) String() string {
	switch s {
	case SortLocal:
		return "local"
	case SortProcess:
		return "process"
	case SortAge:
		return "age"
	case SortTraffic:
		return "traffic"
	case SortConns:
		return "conns"
	default:
		return "remote"
	}
}

// Next returns the following key in the cycle.
func (s SortKey) Next() SortKey {
	for i, k := range sortKeys {
		if k == s {
			return sortKeys[(i+1)%len(sortKeys)]
		}
	}
	return SortRemote
}

// sortKeyNamed maps a persisted sort name back to a key.
func sortKeyNamed(name string) SortKey {
	for _, k := range sortKeys {
		if k.String() == name {
			return k
		}
	}
	return SortRemote
}

// hidePreexisting drops services that were already running when autotun
// connected and are not forwarded. They are the host's own furniture — sshd,
// a database — and listing them by default buries the ports you started.
func hidePreexisting(rows []tunnel.State) []tunnel.State {
	out := rows[:0:0]
	for _, r := range rows {
		if r.Skip == tunnel.SkipPreexising && r.Status != tunnel.StatusActive {
			continue
		}
		out = append(out, r)
	}
	return out
}

// sortStates orders rows in place.
//
// Age and traffic default to descending (newest and busiest first) because that
// is what you want to see without pressing anything; reverse flips whichever
// direction the key considers natural.
func sortStates(rows []tunnel.State, key SortKey, reverse, inactiveLast bool) {
	less := func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch key {
		case SortLocal:
			if a.LocalPort != b.LocalPort {
				return a.LocalPort < b.LocalPort
			}
		case SortProcess:
			if c := strings.Compare(strings.ToLower(a.Cmd), strings.ToLower(b.Cmd)); c != 0 {
				return c < 0
			}
		case SortAge:
			if !a.FirstSeen.Equal(b.FirstSeen) {
				return a.FirstSeen.After(b.FirstSeen)
			}
		case SortTraffic:
			at, bt := a.BytesIn+a.BytesOut, b.BytesIn+b.BytesOut
			if at != bt {
				return at > bt
			}
		case SortConns:
			if a.TotalConns != b.TotalConns {
				return a.TotalConns > b.TotalConns
			}
		}
		return a.RemotePort < b.RemotePort
	}
	ordered := less
	if reverse {
		ordered = func(i, j int) bool { return less(j, i) }
	}
	if inactiveLast {
		// Grouping wraps the chosen order rather than replacing it: live
		// tunnels stay together at the top, sorted among themselves.
		inner := ordered
		ordered = func(i, j int) bool {
			ai := rows[i].Status == tunnel.StatusActive
			bj := rows[j].Status == tunnel.StatusActive
			if ai != bj {
				return ai
			}
			return inner(i, j)
		}
	}
	sort.SliceStable(rows, ordered)
}

// filterStates keeps rows matching a case-insensitive query against the port
// numbers and the process command.
func filterStates(rows []tunnel.State, query string) []tunnel.State {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return rows
	}
	out := rows[:0:0]
	for _, r := range rows {
		if matches(r, q) {
			out = append(out, r)
		}
	}
	return out
}

func matches(r tunnel.State, q string) bool {
	if strings.Contains(strings.ToLower(r.Cmd), q) ||
		strings.Contains(strings.ToLower(r.Proc), q) ||
		strings.Contains(strconv.Itoa(r.RemotePort), q) ||
		strings.Contains(strconv.Itoa(r.LocalPort), q) ||
		strings.Contains(string(r.Status), q) {
		return true
	}
	return false
}
