// Package app wires the SSH transport, the remote prober, the tunnel manager
// and the user interface together.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/jclement/autotun/internal/buildinfo"
	"github.com/jclement/autotun/internal/config"
	"github.com/jclement/autotun/internal/probe"
	"github.com/jclement/autotun/internal/sshx"
	"github.com/jclement/autotun/internal/tunnel"
	"github.com/jclement/autotun/internal/ui"
	"golang.org/x/term"
)

// IO groups the streams the app talks to, so tests can supply their own.
type IO struct {
	In     *os.File
	Out    io.Writer
	Err    io.Writer
	IsTTY  bool
	Width  int
	Height int
}

// StdIO returns the process's real streams.
func StdIO() IO {
	io := IO{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}
	if f, ok := any(os.Stdout).(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		io.IsTTY = true
		io.Width, io.Height, _ = term.GetSize(int(f.Fd()))
	}
	return io
}

// reconnect backoff bounds.
const (
	backoffStart = 1 * time.Second
	backoffMax   = 30 * time.Second
	keepAlive    = 20 * time.Second
)

// Run executes autotun. It returns nil on a clean exit.
func Run(ctx context.Context, cfg Config, iostreams IO) error {
	if cfg.NoColor {
		lipgloss.SetColorProfile(0) // termenv.Ascii
	}

	policy, err := cfg.Validate()
	if err != nil {
		return err
	}
	if cfg.Destination == "" {
		return errors.New("a destination is required; try `autotun --help`")
	}

	sshCfg, err := sshx.LoadConfig()
	if err != nil {
		return err
	}
	dest, err := sshx.Resolve(cfg.Destination, sshCfg, sshx.Overrides{
		User:          cfg.User,
		Port:          cfg.Port,
		IdentityFiles: cfg.IdentityFiles,
		ProxyJump:     cfg.ProxyJump,
	})
	if err != nil {
		return err
	}

	useTUI := iostreams.IsTTY && !cfg.Plain && !cfg.JSON

	var prompter sshx.Prompter
	if p := sshx.NewTerminalPrompter(iostreams.In, iostreams.Err); p != nil {
		prompter = p
	}

	connOpts := sshx.ConnectOptions{
		Prompter:      prompter,
		HostKeyPolicy: cfg.HostKeyPolicy(),
		Timeout:       cfg.ConnectTimeout,
		KeepAlive:     keepAlive,
		Config:        sshCfg,
		ClientVersion: "SSH-2.0-autotun_" + buildinfo.Version(),
	}

	// Connect before the TUI starts so passphrase and host-key prompts have a
	// terminal to use.
	fmt.Fprintf(iostreams.Err, "connecting to %s…\n", dest.String())
	client, err := sshx.Connect(ctx, dest, connOpts)
	if err != nil {
		return err
	}

	// Reconnects happen behind the TUI, where nothing can be prompted for.
	connOpts.Prompter = nil

	alloc := tunnel.NewAllocator(cfg.Bind, cfg.SamePort)

	var renderer Renderer = nopRenderer{}
	switch {
	case useTUI:
	case cfg.JSON:
		renderer = NewJSONRenderer(iostreams.Out)
	default:
		renderer = NewLogRenderer(iostreams.Out)
	}

	// Per-host settings: the view to open in, and each port's protocol,
	// forwarding mode and pinned local port.
	settings := config.OpenDefault()
	host := dest.Label()
	defer func() {
		if err := settings.Save(); err != nil {
			fmt.Fprintln(iostreams.Err, "autotun: could not save host settings:", err)
		}
	}()

	// A remembered view wins unless --existing was passed explicitly.
	if !cfg.Existing && settings.View(host) == config.ViewEverything {
		policy.Existing = true
	}

	var prog *tea.Program
	mgr := tunnel.New(alloc, client, tunnel.Options{
		Policy:        policy,
		Host:          host,
		Settings:      settings,
		DetectSchemes: !cfg.NoDetect,
		OnEvent: func(e tunnel.Event) {
			renderer.Event(e)
		},
	})
	defer mgr.Close()

	sup := &supervisor{
		cfg:      cfg,
		dest:     dest,
		connOpts: connOpts,
		mgr:      mgr,
		renderer: renderer,
		client:   client,
		status: func(msg ui.StatusMsg) {
			if prog != nil {
				prog.Send(msg)
			}
		},
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if !useTUI {
		return runHeadless(ctx, cancel, sup, iostreams)
	}

	model := ui.New(&uiController{Manager: mgr, settings: settings, host: host}, ui.Options{
		Host:     host,
		Version:  buildinfo.Version(),
		Dissolve: !cfg.NoDissolve,
	})
	prog = tea.NewProgram(model,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithContext(ctx),
		tea.WithInput(iostreams.In),
	)

	done := make(chan error, 1)
	go func() { done <- sup.run(ctx) }()

	_, uiErr := prog.Run()
	cancel()
	sup.close()
	<-done

	if uiErr != nil && !errors.Is(uiErr, tea.ErrProgramKilled) && !errors.Is(uiErr, context.Canceled) {
		return uiErr
	}
	return model.Err()
}

// runHeadless drives the app without a TUI. The caller's context carries the
// interrupt signal, so there is nothing to catch here.
func runHeadless(ctx context.Context, cancel context.CancelFunc, sup *supervisor, iostreams IO) error {
	done := make(chan error, 1)
	go func() { done <- sup.run(ctx) }()

	select {
	case err := <-done:
		cancel()
		sup.close()
		return err
	case <-ctx.Done():
		sup.close()
		<-done
		fmt.Fprintln(iostreams.Err, "closing tunnels…")
		return nil
	}
}

// supervisor keeps a probe running against the remote, reconnecting when the
// SSH link drops.
type supervisor struct {
	cfg      Config
	dest     *sshx.Destination
	connOpts sshx.ConnectOptions
	mgr      *tunnel.Manager
	renderer Renderer
	status   func(ui.StatusMsg)

	client *sshx.Client
}

// run owns the connection lifecycle until ctx is canceled.
func (s *supervisor) run(ctx context.Context) error {
	backoff := backoffStart
	attempt := 0

	for ctx.Err() == nil {
		if s.client == nil {
			attempt++
			s.report(ui.StatusMsg{State: ui.Reconnecting, Attempt: attempt})
			c, err := sshx.Connect(ctx, s.dest, s.connOpts)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				s.report(ui.StatusMsg{State: ui.Disconnected, Attempt: attempt, Detail: err.Error()})
				if !sleepCtx(ctx, backoff) {
					return nil
				}
				backoff = nextBackoff(backoff)
				continue
			}
			s.client = c
			s.mgr.SetDialer(c)
		}

		attempt = 0
		backoff = backoffStart

		mon := probe.NewMonitor(s.client, s.cfg.Interval)
		err := mon.Run(ctx,
			func(info probe.Info) {
				s.report(ui.StatusMsg{State: ui.Connected, Mode: string(info.Mode)})
			},
			func(res probe.Result) {
				s.mgr.Sync(res.Snapshot)
			},
		)

		s.mgr.SetDialer(nil)
		if s.client != nil {
			s.client.Close()
			s.client = nil
		}
		if ctx.Err() != nil {
			return nil
		}

		detail := "connection closed"
		if err != nil && !errors.Is(err, io.EOF) {
			detail = err.Error()
		}
		// A prober that fails immediately every time is a configuration
		// problem, not a flaky link — surface it rather than looping in
		// silence.
		s.report(ui.StatusMsg{State: ui.Disconnected, Detail: detail})
		if !sleepCtx(ctx, backoff) {
			return nil
		}
		backoff = nextBackoff(backoff)
	}
	return nil
}

func (s *supervisor) report(msg ui.StatusMsg) {
	if s.status != nil {
		s.status(msg)
	}
	s.renderer.Status(msg.State, msg.Detail)
}

func (s *supervisor) close() {
	if s.client != nil {
		s.client.Close()
	}
}

func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > backoffMax {
		return backoffMax
	}
	return d
}

// sleepCtx waits for d, reporting false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// uiController adapts the tunnel manager for the UI, persisting the choices
// that belong to the host rather than to a single run.
type uiController struct {
	*tunnel.Manager
	settings *config.Store
	host     string
}

// SetPolicy records a view change so the host opens the same way next time.
func (c *uiController) SetPolicy(p tunnel.Policy) {
	view := config.ViewSinceStart
	if p.Existing {
		view = config.ViewEverything
	}
	c.settings.SetView(c.host, view)
	c.Manager.SetPolicy(p)
}
