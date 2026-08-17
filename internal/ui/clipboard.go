package ui

import "encoding/base64"

// OSC52 builds the terminal escape sequence that sets the system clipboard.
//
// This is the only clipboard mechanism that works from inside an SSH session:
// the sequence travels back over the same terminal stream the UI is drawn on,
// and the local terminal emulator performs the copy. No X11, no pbcopy, no cgo.
func OSC52(s string) string {
	return "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(s)) + "\x07"
}
