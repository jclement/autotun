package buildinfo

import (
	"strings"
	"testing"
)

func TestVersionFallsBackToDev(t *testing.T) {
	// The package-level vars are only set by the release build, so a plain
	// `go test` must still produce something sensible.
	if got := Version(); got == "" {
		t.Error("Version() is empty")
	}
}

func TestVersionUsesTheStampedValue(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })

	version = "v1.2.3"
	if got := Version(); got != "v1.2.3" {
		t.Errorf("Version() = %q, want the stamped value", got)
	}
}

func TestFullIncludesTheStampedMetadata(t *testing.T) {
	oldV, oldC, oldD := version, commit, date
	t.Cleanup(func() { version, commit, date = oldV, oldC, oldD })

	version, commit, date = "v1.2.3", "0123456789abcdef", "2026-08-17"

	got := Full()
	if !strings.Contains(got, "v1.2.3") {
		t.Errorf("Full() = %q, want the version", got)
	}
	// The commit is abbreviated the way git does.
	if !strings.Contains(got, "(0123456)") {
		t.Errorf("Full() = %q, want a 7-character commit", got)
	}
	if !strings.Contains(got, "2026-08-17") {
		t.Errorf("Full() = %q, want the build date", got)
	}
}

func TestFullWithNothingStamped(t *testing.T) {
	oldV, oldC, oldD := version, commit, date
	t.Cleanup(func() { version, commit, date = oldV, oldC, oldD })

	version, commit, date = "v1.0.0", "", ""
	if got := Full(); got != "v1.0.0" {
		t.Errorf("Full() = %q, want just the version", got)
	}
}

func TestShortCommitIsNotTruncatedTwice(t *testing.T) {
	oldV, oldC := version, commit
	t.Cleanup(func() { version, commit = oldV, oldC })

	version, commit = "v1.0.0", "abc"
	if got := Full(); !strings.Contains(got, "(abc)") {
		t.Errorf("Full() = %q, want the short commit unchanged", got)
	}
}

func TestCommitAndDate(t *testing.T) {
	oldC, oldD := commit, date
	t.Cleanup(func() { commit, date = oldC, oldD })

	commit, date = "deadbeef", "2026-01-01"
	if got := Commit(); got != "deadbeef" {
		t.Errorf("Commit() = %q", got)
	}
	if got := Date(); got != "2026-01-01" {
		t.Errorf("Date() = %q", got)
	}
}

// Build tooling stamps "none"/"unknown" when it has nothing real; those must
// not reach the user as "autotun v1.0.0 (none)".
func TestFullIgnoresPlaceholderStamps(t *testing.T) {
	oldV, oldC, oldD := version, commit, date
	t.Cleanup(func() { version, commit, date = oldV, oldC, oldD })

	version, commit, date = "v1.0.0", "none", "unknown"
	if got := Full(); got != "v1.0.0" {
		t.Errorf("Full() = %q, want the placeholders dropped", got)
	}
	if got := Commit(); got != "" {
		t.Errorf("Commit() = %q, want empty for a placeholder", got)
	}
	if got := Date(); got != "" {
		t.Errorf("Date() = %q, want empty for a placeholder", got)
	}
}
