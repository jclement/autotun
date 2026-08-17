package sshx

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// HostKeyPolicy controls what happens when a host key is unknown or has
// changed. It mirrors ssh(1)'s StrictHostKeyChecking.
type HostKeyPolicy string

const (
	// HostKeyAsk prompts before trusting an unknown host.
	HostKeyAsk HostKeyPolicy = "ask"
	// HostKeyAcceptNew trusts and records unknown hosts without asking, but
	// still refuses a host whose key has changed.
	HostKeyAcceptNew HostKeyPolicy = "accept-new"
	// HostKeyStrict refuses anything not already in known_hosts.
	HostKeyStrict HostKeyPolicy = "yes"
	// HostKeyNone disables verification entirely.
	HostKeyNone HostKeyPolicy = "no"
)

// ParseHostKeyPolicy maps an ssh_config StrictHostKeyChecking value.
func ParseHostKeyPolicy(s string) HostKeyPolicy {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "yes", "true":
		return HostKeyStrict
	case "no", "false", "off":
		return HostKeyNone
	case "accept-new":
		return HostKeyAcceptNew
	default:
		return HostKeyAsk
	}
}

// Prompter asks the user questions before the TUI takes over the terminal.
type Prompter interface {
	// Confirm asks a yes/no question. It must return false when there is no
	// terminal to ask on.
	Confirm(question string) (bool, error)
	// Secret reads a line without echoing it.
	Secret(prompt string) (string, error)
	// Line reads an echoed line.
	Line(prompt string) (string, error)
	// Notice prints an informational message.
	Notice(msg string)
}

// ErrHostKeyRejected is returned when the user declines an unknown host key.
var ErrHostKeyRejected = errors.New("host key rejected")

// knownHostsPaths returns the known_hosts files to consult, creating the user's
// file if it does not exist so it can be appended to later.
func knownHostsPaths() []string {
	var paths []string
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	primary := filepath.Join(home, ".ssh", "known_hosts")
	if _, err := os.Stat(primary); err != nil {
		if err := os.MkdirAll(filepath.Dir(primary), 0o700); err == nil {
			if f, err := os.OpenFile(primary, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				f.Close()
			}
		}
	}
	if _, err := os.Stat(primary); err == nil {
		paths = append(paths, primary)
	}
	if p := filepath.Join(home, ".ssh", "known_hosts2"); fileExists(p) {
		paths = append(paths, p)
	}
	return paths
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// HostKeyCallback builds a verification callback implementing policy, using
// prompter for the interactive cases.
func HostKeyCallback(policy HostKeyPolicy, prompter Prompter) (ssh.HostKeyCallback, error) {
	if policy == HostKeyNone {
		return ssh.InsecureIgnoreHostKey(), nil //nolint:gosec // explicitly requested
	}
	paths := knownHostsPaths()
	if len(paths) == 0 {
		return nil, errors.New("no known_hosts file available; use --insecure-host-key to skip verification")
	}
	verify, err := knownhosts.New(paths...)
	if err != nil {
		return nil, fmt.Errorf("reading known_hosts: %w", err)
	}

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := verify(hostname, remote, key)
		if err == nil {
			return nil
		}

		var ke *knownhosts.KeyError
		if !errors.As(err, &ke) {
			return err
		}
		// A populated Want list means the host is known but presented a
		// different key. That is never auto-accepted.
		if len(ke.Want) > 0 {
			return fmt.Errorf(
				"REMOTE HOST IDENTIFICATION HAS CHANGED for %s\n"+
					"  offered %s key %s\n"+
					"This may be a man-in-the-middle attack, or the host may have been rebuilt.\n"+
					"If you trust the change, remove the old key with:\n"+
					"  ssh-keygen -R %s",
				hostname, key.Type(), ssh.FingerprintSHA256(key), knownhosts.Normalize(hostname))
		}

		switch policy {
		case HostKeyStrict:
			return fmt.Errorf("host %s is not in known_hosts and --strict-host-key is set", hostname)
		case HostKeyAsk:
			if prompter == nil {
				return fmt.Errorf("host %s is not in known_hosts and there is no terminal to confirm on", hostname)
			}
			prompter.Notice(fmt.Sprintf(
				"The authenticity of host '%s' can't be established.\n%s key fingerprint is %s.",
				hostname, key.Type(), ssh.FingerprintSHA256(key)))
			ok, cerr := prompter.Confirm("Are you sure you want to continue connecting?")
			if cerr != nil {
				return cerr
			}
			if !ok {
				return ErrHostKeyRejected
			}
		case HostKeyAcceptNew:
			if prompter != nil {
				prompter.Notice(fmt.Sprintf("Permanently added '%s' (%s) to known hosts.", hostname, key.Type()))
			}
		}
		return appendKnownHost(paths[0], hostname, remote, key)
	}, nil
}

// appendKnownHost records a newly trusted key.
func appendKnownHost(path, hostname string, remote net.Addr, key ssh.PublicKey) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("recording host key: %w", err)
	}
	defer f.Close()

	addrs := []string{knownhosts.Normalize(hostname)}
	if remote != nil {
		if n := knownhosts.Normalize(remote.String()); n != addrs[0] {
			addrs = append(addrs, n)
		}
	}
	if _, err := f.WriteString(knownhosts.Line(addrs, key) + "\n"); err != nil {
		return fmt.Errorf("recording host key: %w", err)
	}
	return nil
}
