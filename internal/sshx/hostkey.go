package sshx

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
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
		// Only a recorded key of the *same* algorithm can contradict this
		// one. Records of other algorithms mean the host has a key type we
		// have not seen before, which is a new key, not a changed one.
		if changed := keysOfType(ke.Want, key); len(changed) > 0 {
			return fmt.Errorf(
				"REMOTE HOST IDENTIFICATION HAS CHANGED for %s\n"+
					"  offered  %s key %s\n"+
					"  recorded %s key %s (%s:%d)\n"+
					"This may be a man-in-the-middle attack, or the host may have been rebuilt.\n"+
					"If you trust the change, remove the old key with:\n"+
					"  ssh-keygen -R %s",
				hostname, key.Type(), ssh.FingerprintSHA256(key),
				changed[0].Key.Type(), ssh.FingerprintSHA256(changed[0].Key),
				changed[0].Filename, changed[0].Line,
				knownhosts.Normalize(hostname))
		}

		switch policy {
		case HostKeyStrict:
			return fmt.Errorf("host %s is not in known_hosts and --strict-host-key is set", hostname)
		case HostKeyAsk:
			if prompter == nil {
				return fmt.Errorf("host %s is not in known_hosts and there is no terminal to confirm on", hostname)
			}
			notice := fmt.Sprintf(
				"The authenticity of host '%s' can't be established.\n%s key fingerprint is %s.",
				hostname, key.Type(), ssh.FingerprintSHA256(key))
			if len(ke.Want) > 0 {
				notice += fmt.Sprintf("\nThis host is already known by a different key type (%s).", ke.Want[0].Key.Type())
			}
			prompter.Notice(notice)
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

// ---- Algorithm preference ----

// textAddr carries a host:port string for a known_hosts lookup made outside
// any live connection.
type textAddr string

func (a textAddr) Network() string { return "tcp" }
func (a textAddr) String() string  { return string(a) }

// recordedKeys returns every key known_hosts holds for addr ("host:port").
// There is no lookup API, so it asks the ordinary callback about a key no
// host could possibly be using: the resulting KeyError lists what it wanted
// instead, which is exactly the set of recorded keys.
func recordedKeys(addr string) []knownhosts.KnownKey {
	paths := knownHostsPaths()
	if len(paths) == 0 {
		return nil
	}
	verify, err := knownhosts.New(paths...)
	if err != nil {
		return nil
	}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil
	}
	probe, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil
	}
	var ke *knownhosts.KeyError
	if errors.As(verify(addr, textAddr(addr), probe), &ke) {
		return ke.Want
	}
	return nil
}

// signatureAlgos expands a recorded key type into the host key algorithms
// that can carry it. RSA keys are recorded as plain "ssh-rsa" but every
// current server signs with a SHA-2 variant, so all three have to be offered
// for the one recorded key to be usable.
func signatureAlgos(keyType string) []string {
	switch keyType {
	case ssh.KeyAlgoRSA:
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	case ssh.CertAlgoRSAv01:
		return []string{ssh.CertAlgoRSASHA512v01, ssh.CertAlgoRSASHA256v01, ssh.CertAlgoRSAv01}
	default:
		return []string{keyType}
	}
}

// HostKeyAlgorithms returns the host key algorithms to advertise when
// connecting to addr ("host:port"): the ones already in known_hosts first,
// then the rest.
//
// Without this the client offers its own global preference — ECDSA ahead of
// Ed25519 — and a server holding both will hand back the ECDSA key even
// though known_hosts records the Ed25519 one. The host is then reported as
// unrecognized, or worse as changed, for no reason at all. ssh(1) avoids that
// by ordering the algorithms it asks for by what it already trusts, and so do
// we. Everything else stays on the list behind them, so a host that genuinely
// rotated to a new key type still negotiates and gets the honest "this key is
// new" conversation instead of failing to agree on an algorithm.
func HostKeyAlgorithms(policy HostKeyPolicy, addr string) []string {
	if policy == HostKeyNone {
		return nil
	}
	var algos []string
	add := func(candidates []string) {
		for _, algo := range candidates {
			if !slices.Contains(algos, algo) {
				algos = append(algos, algo)
			}
		}
	}
	for _, known := range recordedKeys(addr) {
		add(signatureAlgos(known.Key.Type()))
	}
	if len(algos) == 0 {
		return nil // Nothing recorded: no opinion, leave the default order.
	}
	add(ssh.SupportedAlgorithms().HostKeys)
	add(ssh.InsecureAlgorithms().HostKeys)
	return algos
}

// keysOfType picks out the recorded keys using the same algorithm as key.
// Only those can say anything about whether the host's identity changed;
// a recorded key of a different type is simply a different key.
func keysOfType(known []knownhosts.KnownKey, key ssh.PublicKey) []knownhosts.KnownKey {
	var same []knownhosts.KnownKey
	for _, k := range known {
		if k.Key.Type() == key.Type() {
			same = append(same, k)
		}
	}
	return same
}
