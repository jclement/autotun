//go:build windows

package sshx

import (
	"net"
	"os"
	"time"

	"github.com/Microsoft/go-winio"
)

// windowsAgentPipe is the named pipe used by the OpenSSH agent service shipped
// with Windows.
const windowsAgentPipe = `\\.\pipe\openssh-ssh-agent`

// dialAgentConn connects to the Windows OpenSSH agent. SSH_AUTH_SOCK is
// honored first for users running a Unix-socket agent under Git Bash or WSL
// interop, then the standard named pipe.
func dialAgentConn() (net.Conn, error) {
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			return conn, nil
		}
	}
	timeout := 2 * time.Second
	return winio.DialPipe(windowsAgentPipe, &timeout)
}
