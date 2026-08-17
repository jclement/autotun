package main

import (
	"os"
	"testing"
)

// withArgs runs run() with a synthetic command line.
func withArgs(t *testing.T, args ...string) int {
	t.Helper()
	old := os.Args
	t.Cleanup(func() { os.Args = old })
	os.Args = append([]string{"autotun"}, args...)
	return run()
}

func TestRunPrintsVersion(t *testing.T) {
	if code := withArgs(t, "--version"); code != 0 {
		t.Errorf("--version exited %d, want 0", code)
	}
	if code := withArgs(t, "-V"); code != 0 {
		t.Errorf("-V exited %d, want 0", code)
	}
}

func TestRunShowsHelp(t *testing.T) {
	if code := withArgs(t, "--help"); code != 0 {
		t.Errorf("--help exited %d, want 0", code)
	}
}

func TestRunRequiresADestination(t *testing.T) {
	if code := withArgs(t); code != 2 {
		t.Errorf("no arguments exited %d, want 2 (usage error)", code)
	}
}

func TestRunRejectsMultipleDestinations(t *testing.T) {
	if code := withArgs(t, "host1", "host2"); code != 2 {
		t.Errorf("two destinations exited %d, want 2", code)
	}
}

func TestRunRejectsUnknownFlags(t *testing.T) {
	if code := withArgs(t, "--not-a-flag", "devbox"); code != 2 {
		t.Errorf("an unknown flag exited %d, want 2", code)
	}
}

func TestRunReportsConfigErrors(t *testing.T) {
	// A validation failure exits 1, distinct from a usage error.
	if code := withArgs(t, "--min-port", "9000", "--max-port", "80", "devbox"); code != 1 {
		t.Errorf("an invalid port window exited %d, want 1", code)
	}
}
