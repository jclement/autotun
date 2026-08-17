//go:build !windows

package sshx

import (
	"errors"
	"net"
	"os"
)

// dialAgentConn connects to the agent socket named by SSH_AUTH_SOCK.
func dialAgentConn() (net.Conn, error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, errors.New("SSH_AUTH_SOCK is not set")
	}
	return net.Dial("unix", sock)
}
