// Package buildinfo exposes the version stamped in at release time.
package buildinfo

import "runtime/debug"

// These are set by GoReleaser via -ldflags.
var (
	version = ""
	commit  = ""
	date    = ""
)

// Version returns the release version, falling back to the module version
// recorded by `go install`, then to "dev".
func Version() string {
	if !placeholder(version) {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// placeholder reports whether a stamped value is one of the sentinels build
// tooling substitutes when it has nothing real to put there.
func placeholder(s string) bool {
	switch s {
	case "", "none", "unknown", "n/a":
		return true
	}
	return false
}

// Commit returns the git commit, if known.
func Commit() string {
	if !placeholder(commit) {
		return commit
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return ""
}

// Date returns the build date, if known.
func Date() string {
	if placeholder(date) {
		return ""
	}
	return date
}

// Full renders a one-line version string.
func Full() string {
	s := Version()
	if c := Commit(); c != "" {
		if len(c) > 7 {
			c = c[:7]
		}
		s += " (" + c + ")"
	}
	if d := Date(); d != "" {
		s += " built " + d
	}
	return s
}
