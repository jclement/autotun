package sshx

import (
	"bytes"
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

// authMethods assembles the authentication methods to offer.
//
// Ordering matters more than it looks: every public key offered and refused
// counts against the server's MaxAuthTries, so a connection can fail with "no
// supported methods remain" while holding a key that would have worked. The
// order therefore mirrors ssh(1):
//
//   - identities named explicitly (-i, or IdentityFile in ssh_config) go first,
//     because naming one is a statement about which key to use;
//   - otherwise the agent goes first, since a key held in an agent — a YubiKey,
//     a passphrase already entered — is far likelier to be the live one than a
//     forgotten id_rsa left in ~/.ssh years ago.
func authMethods(d *Destination, prompter Prompter) ([]ssh.AuthMethod, []string, func(), error) {
	var methods []ssh.AuthMethod
	var cleanup []func()
	// offered describes, in order, what will be presented to the server. It is
	// reported back on failure: "the server refused every key" is only useful
	// if you can see which keys those were.
	var offered []string

	closeAll := func() {
		for _, c := range cleanup {
			c()
		}
	}

	// An identity named in the configuration is a deliberate choice; the
	// defaults found by scanning ~/.ssh are just what happens to be there.
	named := len(d.IdentityFiles) > 0
	keyFiles := d.IdentityFiles
	if !named {
		keyFiles = discoverKeys()
	}

	var agentMethod ssh.AuthMethod
	var agentBlobs map[string]bool
	agentDesc := ""
	if !d.IdentitiesOnly {
		if ag, closer, err := dialAgent(); err == nil {
			agentMethod = ssh.PublicKeysCallback(agentSigners(ag))
			agentBlobs = agentPublicKeys(ag)
			agentDesc = fmt.Sprintf("agent (%d key%s)", len(agentBlobs), plural(len(agentBlobs)))
			cleanup = append(cleanup, closer)
		}
	}

	var fileDesc []string
	fileMethod := func() ssh.AuthMethod {
		var signers []ssh.Signer
		for _, path := range keyFiles {
			// Offering a key the agent already holds wastes an attempt on a
			// credential that is about to be offered anyway.
			if !named && agentBlobs != nil && agentHasKeyFor(agentBlobs, path) {
				continue
			}
			signer, err := loadKey(path, prompter)
			if err != nil {
				if prompter != nil && !errors.Is(err, os.ErrNotExist) {
					prompter.Notice(fmt.Sprintf("skipping %s: %v", path, err))
				}
				continue
			}
			signers = append(signers, signer)
			fileDesc = append(fileDesc, path)
		}
		if len(signers) == 0 {
			return nil
		}
		return ssh.PublicKeys(signers...)
	}

	if named {
		if m := fileMethod(); m != nil {
			methods = append(methods, m)
			offered = append(offered, fileDesc...)
		}
		if agentMethod != nil {
			methods = append(methods, agentMethod)
			offered = append(offered, agentDesc)
		}
	} else {
		if agentMethod != nil {
			methods = append(methods, agentMethod)
			offered = append(offered, agentDesc)
		}
		if m := fileMethod(); m != nil {
			methods = append(methods, m)
			offered = append(offered, fileDesc...)
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
		return nil, nil, nil, errors.New("no usable authentication methods (no agent, no keys, no terminal to prompt on)")
	}
	return methods, offered, closeAll, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// dialAgent connects to the running SSH agent, if any.
func dialAgent() (agent.ExtendedAgent, func(), error) {
	conn, err := dialAgentConn()
	if err != nil {
		return nil, nil, err
	}
	return agent.NewClient(conn), func() { _ = conn.Close() }, nil
}

// agentSigners fetches the agent's signers, retrying once if the first attempt
// comes back empty or failing.
//
// This is not defensive padding. gpg-agent backed by a smartcard — a YubiKey —
// can answer the first identity request before it has scanned the card, giving
// back an error or an empty list. Offering nothing makes the publickey method
// fail outright, and the connection dies with "no supported methods remain"
// while the key that would have worked sits in the agent.
func agentSigners(ag agent.ExtendedAgent) func() ([]ssh.Signer, error) {
	return func() ([]ssh.Signer, error) {
		signers, err := ag.Signers()
		if err == nil && len(signers) > 0 {
			return signers, nil
		}
		retry, retryErr := ag.Signers()
		if retryErr != nil {
			if err != nil {
				return nil, err
			}
			return nil, retryErr
		}
		return retry, nil
	}
}

// agentPublicKeys indexes the public keys an agent is holding. Listing does not
// touch the hardware; only signing does, so this is safe with a YubiKey. It
// doubles as the first request that wakes a smartcard-backed agent.
func agentPublicKeys(ag agent.ExtendedAgent) map[string]bool {
	keys, err := ag.List()
	if err != nil || len(keys) == 0 {
		if keys, err = ag.List(); err != nil {
			return nil
		}
	}
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[string(k.Marshal())] = true
	}
	return out
}

// agentHasKeyFor reports whether the agent already holds the key whose public
// half sits beside the given private key file.
func agentHasKeyFor(blobs map[string]bool, privatePath string) bool {
	data, err := os.ReadFile(privatePath + ".pub")
	if err != nil {
		return false
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey(bytes.TrimSpace(data))
	if err != nil {
		return false
	}
	return blobs[string(pub.Marshal())]
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
