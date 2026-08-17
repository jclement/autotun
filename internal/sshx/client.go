package sshx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// ConnectOptions tunes how a connection is established.
type ConnectOptions struct {
	Prompter      Prompter
	HostKeyPolicy HostKeyPolicy
	// Timeout bounds the TCP connect and SSH handshake.
	Timeout time.Duration
	// KeepAlive is how often to probe a live connection. Zero disables it.
	KeepAlive time.Duration
	// Config, if set, resolves ProxyJump hops against ssh_config.
	Config configGetter
	// ClientVersion is advertised in the SSH banner.
	ClientVersion string
}

// Client is a live SSH connection. It satisfies both probe.Runner and
// tunnel.Dialer, so the whole app runs over this one transport.
type Client struct {
	ssh  *ssh.Client
	dest *Destination

	closeOnce sync.Once
	closed    chan struct{}
	extra     []func()

	// wait is closed when the underlying connection drops.
	wait     chan struct{}
	waitOnce sync.Once
	waitErr  error
}

// Connect dials the destination, following any ProxyJump chain.
func Connect(ctx context.Context, d *Destination, opts ConnectOptions) (*Client, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 20 * time.Second
	}
	if opts.ClientVersion == "" {
		opts.ClientVersion = "SSH-2.0-autotun"
	}
	if d.StrictHostKey != "" && opts.HostKeyPolicy == "" {
		opts.HostKeyPolicy = ParseHostKeyPolicy(d.StrictHostKey)
	}
	if opts.HostKeyPolicy == "" {
		opts.HostKeyPolicy = HostKeyAsk
	}

	var closers []func()
	fail := func(err error) (*Client, error) {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
		return nil, err
	}

	var via *ssh.Client
	for _, hop := range parseJumpChain(d.ProxyJump) {
		hd, err := Resolve(hop, opts.Config, Overrides{})
		if err != nil {
			return fail(fmt.Errorf("proxy jump %q: %w", hop, err))
		}
		c, err := dialSSH(ctx, hd, opts, via)
		if err != nil {
			return fail(fmt.Errorf("proxy jump %s: %w", hd.Label(), err))
		}
		via = c
		closers = append(closers, func() { _ = c.Close() })
	}

	sc, err := dialSSH(ctx, d, opts, via)
	if err != nil {
		return fail(err)
	}

	c := &Client{ssh: sc, dest: d, closed: make(chan struct{}), wait: make(chan struct{}), extra: closers}
	go c.watch()
	if opts.KeepAlive > 0 {
		go c.keepalive(opts.KeepAlive)
	}
	return c, nil
}

// dialSSH performs one hop, optionally tunneled through via.
func dialSSH(ctx context.Context, d *Destination, opts ConnectOptions, via *ssh.Client) (*ssh.Client, error) {
	hostKey, err := HostKeyCallback(opts.HostKeyPolicy, opts.Prompter)
	if err != nil {
		return nil, err
	}
	methods, offered, cleanup, err := authMethods(d, opts.Prompter)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	cfg := &ssh.ClientConfig{
		User:            d.User,
		Auth:            methods,
		HostKeyCallback: hostKey,
		Timeout:         opts.Timeout,
		ClientVersion:   opts.ClientVersion,
	}

	var conn net.Conn
	if via != nil {
		conn, err = via.DialContext(ctx, "tcp", d.Addr())
	} else {
		dialer := &net.Dialer{Timeout: opts.Timeout}
		conn, err = dialer.DialContext(ctx, "tcp", d.Addr())
	}
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", d.Addr(), err)
	}

	// The handshake has no context of its own, so bound it with a deadline
	// and clear it once the connection is up.
	_ = conn.SetDeadline(time.Now().Add(opts.Timeout))
	sconn, chans, reqs, err := ssh.NewClientConn(conn, d.Addr(), cfg)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("ssh handshake with %s: %w%s", d.Label(), err, authHint(err, d, offered))
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(sconn, chans, reqs), nil
}

// authHint adds guidance to an authentication failure, which is otherwise one
// of the least actionable errors SSH produces.
func authHint(err error, d *Destination, offered []string) string {
	if !strings.Contains(err.Error(), "unable to authenticate") {
		return ""
	}
	hint := "\n\nThe server refused every credential offered, in this order:"
	if len(offered) == 0 {
		hint += "\n  (none)"
	}
	for _, o := range offered {
		hint += "\n  - " + o
	}
	if d.IdentitiesOnly {
		hint += "\n\nThe agent was not consulted: naming a key with -i, or IdentitiesOnly in" +
			"\nssh_config, restricts autotun to the keys named there."
	} else {
		hint += "\n\nagent keys are offered first, then the ~/.ssh defaults."
	}
	hint += "\nIf `ssh " + d.Label() + "` works, check that the agent holding that key is"
	hint += "\nreachable here (ssh-add -l), or name the key directly with -i."
	return hint
}

// parseJumpChain splits a ProxyJump value into hops, nearest first.
func parseJumpChain(s string) []string {
	var out []string
	for _, hop := range strings.Split(s, ",") {
		if hop = strings.TrimSpace(hop); hop != "" {
			out = append(out, hop)
		}
	}
	return out
}

// watch closes Wait when the transport dies.
func (c *Client) watch() {
	err := c.ssh.Wait()
	c.waitOnce.Do(func() {
		c.waitErr = err
		close(c.wait)
	})
}

// keepalive sends periodic global requests so a silently dropped link is
// noticed rather than hanging until a TCP timeout hours later.
func (c *Client) keepalive(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	misses := 0
	for {
		select {
		case <-c.closed:
			return
		case <-c.wait:
			return
		case <-t.C:
			_, _, err := c.ssh.SendRequest("keepalive@openssh.com", true, nil)
			if err == nil {
				misses = 0
				continue
			}
			if misses++; misses >= 3 {
				c.Close()
				return
			}
		}
	}
}

// Wait returns a channel closed when the connection drops.
func (c *Client) Wait() <-chan struct{} { return c.wait }

// Err returns why the connection ended, once Wait has fired.
func (c *Client) Err() error { return c.waitErr }

// Dest returns the resolved destination.
func (c *Client) Dest() *Destination { return c.dest }

// Dial opens a connection from the remote host, satisfying tunnel.Dialer.
func (c *Client) Dial(network, addr string) (net.Conn, error) {
	return c.ssh.Dial(network, addr)
}

// Run pipes a script into the remote `sh -s` and streams its stdout, satisfying
// probe.Runner. It returns when the command exits or ctx is canceled.
func (c *Client) Run(ctx context.Context, script string, stdout io.Writer) error {
	sess, err := c.ssh.NewSession()
	if err != nil {
		return fmt.Errorf("opening remote session: %w", err)
	}
	defer sess.Close()

	sess.Stdin = strings.NewReader(script)
	sess.Stdout = stdout
	var stderr bytes.Buffer
	sess.Stderr = &stderr

	// `sh -s` is chosen over embedding the script in the command string so
	// that quoting is never routed through the remote login shell, which may
	// be fish, csh or something stranger.
	if err := sess.Start("sh -s"); err != nil {
		return fmt.Errorf("starting remote prober: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()

	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGTERM)
		_ = sess.Close()
		<-done
		return ctx.Err()
	case <-c.wait:
		return fmt.Errorf("connection lost: %w", c.waitErr)
	case err := <-done:
		if err != nil {
			if msg := strings.TrimSpace(stderr.String()); msg != "" {
				return fmt.Errorf("remote prober: %s", msg)
			}
			return fmt.Errorf("remote prober: %w", err)
		}
		return nil
	}
}

// Output runs a short-lived script and returns its stdout.
func (c *Client) Output(ctx context.Context, script string) (string, error) {
	var buf bytes.Buffer
	err := c.Run(ctx, script, &buf)
	return buf.String(), err
}

// Close tears down the connection and any proxy hops.
func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.closed)
		err = c.ssh.Close()
		for i := len(c.extra) - 1; i >= 0; i-- {
			c.extra[i]()
		}
		c.waitOnce.Do(func() { close(c.wait) })
	})
	return err
}
