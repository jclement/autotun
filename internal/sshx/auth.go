package sshx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// defaultKeyNames are the identity files ssh(1) tries when none is configured,
// in the same order.
var defaultKeyNames = []string{
	"id_ed25519",
	"id_ecdsa",
	"id_ecdsa_sk",
	"id_ed25519_sk",
	"id_rsa",
	"id_dsa",
}

// authMethods assembles the authentication methods to offer, in preference
// order: agent, then explicit/discovered keys, then interactive fallbacks.
func authMethods(d *Destination, prompter Prompter) ([]ssh.AuthMethod, func(), error) {
	var methods []ssh.AuthMethod
	var cleanup []func()

	closeAll := func() {
		for _, c := range cleanup {
			c()
		}
	}

	// Named keys go first. Every failed public key counts against the
	// server's MaxAuthTries, so offering an agent's worth of unrelated keys
	// ahead of the one the user asked for is how a connection fails with
	// "no supported methods remain" despite a perfectly good key.
	keyFiles := d.IdentityFiles
	if len(keyFiles) == 0 {
		keyFiles = discoverKeys()
	}
	var signers []ssh.Signer
	for _, path := range keyFiles {
		signer, err := loadKey(path, prompter)
		if err != nil {
			if prompter != nil && !errors.Is(err, os.ErrNotExist) {
				prompter.Notice(fmt.Sprintf("skipping %s: %v", path, err))
			}
			continue
		}
		signers = append(signers, signer)
	}
	if len(signers) > 0 {
		methods = append(methods, ssh.PublicKeys(signers...))
	}

	if !d.IdentitiesOnly {
		if ag, closer, err := dialAgent(); err == nil {
			methods = append(methods, ssh.PublicKeysCallback(ag.Signers))
			cleanup = append(cleanup, closer)
		}
	}

	if prompter != nil {
		methods = append(methods,
			ssh.KeyboardInteractive(func(name, instruction string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i, q := range questions {
					var err error
					if echos[i] {
						answers[i], err = prompter.Line(q)
					} else {
						answers[i], err = prompter.Secret(q)
					}
					if err != nil {
						return nil, err
					}
				}
				return answers, nil
			}),
			ssh.PasswordCallback(func() (string, error) {
				return prompter.Secret(fmt.Sprintf("%s@%s's password: ", d.User, d.Host))
			}),
		)
	}

	if len(methods) == 0 {
		closeAll()
		return nil, nil, errors.New("no usable authentication methods (no agent, no keys, no terminal to prompt on)")
	}
	return methods, closeAll, nil
}

// dialAgent connects to the running SSH agent, if any.
func dialAgent() (agent.ExtendedAgent, func(), error) {
	conn, err := dialAgentConn()
	if err != nil {
		return nil, nil, err
	}
	return agent.NewClient(conn), func() { conn.Close() }, nil
}

// discoverKeys returns the default identity files that exist.
func discoverKeys() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var out []string
	for _, name := range defaultKeyNames {
		p := filepath.Join(home, ".ssh", name)
		if fileExists(p) {
			out = append(out, p)
		}
	}
	return out
}

// loadKey parses a private key, prompting for a passphrase if it is encrypted.
func loadKey(path string, prompter Prompter) (ssh.Signer, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(pem)
	if err == nil {
		return signer, nil
	}

	var pass *ssh.PassphraseMissingError
	if !errors.As(err, &pass) {
		return nil, err
	}
	if prompter == nil {
		return nil, fmt.Errorf("key is passphrase-protected and there is no terminal to prompt on")
	}
	phrase, err := prompter.Secret(fmt.Sprintf("Enter passphrase for key %s: ", path))
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKeyWithPassphrase(pem, []byte(phrase))
}
