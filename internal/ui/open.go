package ui

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// OpenURL launches the platform's default browser.
func OpenURL(url string) error {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("refusing to open non-http URL %q", url)
	}
	name, args := openCommand(url)
	if name == "" {
		return fmt.Errorf("don't know how to open a browser on %s", runtime.GOOS)
	}
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reap the child so it does not linger as a zombie for the session.
	go func() { _ = cmd.Wait() }()
	return nil
}

// openCommand returns the platform opener. Split out so it is testable without
// launching anything.
func openCommand(url string) (string, []string) {
	switch runtime.GOOS {
	case "darwin":
		return "open", []string{url}
	case "windows":
		// The empty first argument is the window title, which `start`
		// otherwise steals from the URL.
		return "cmd", []string{"/c", "start", "", url}
	default:
		return "xdg-open", []string{url}
	}
}
