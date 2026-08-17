// Command autotun forwards every port a remote development host opens, so they
// show up on localhost automatically.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jclement/autotun/internal/app"
	"github.com/jclement/autotun/internal/buildinfo"
	"github.com/jclement/autotun/internal/selfupdate"
	"github.com/spf13/pflag"
)

func main() {
	os.Exit(run())
}

func run() int {
	// `update` is the one subcommand; everything else is a destination plus
	// flags, so it is dispatched before the main flag set is parsed.
	if len(os.Args) > 1 && os.Args[1] == "update" {
		return runUpdate(os.Args[2:])
	}

	var cfg app.Config
	fs := cfg.Flags("autotun", os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, app.Usage)
		fmt.Fprintln(os.Stderr, fs.FlagUsages())
	}

	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return 0
		}
		return 2
	}

	if cfg.Version {
		fmt.Println("autotun", buildinfo.Full())
		return 0
	}

	switch args := fs.Args(); len(args) {
	case 0:
		fs.Usage()
		return 2
	case 1:
		cfg.Destination = args[0]
	default:
		fmt.Fprintf(os.Stderr, "autotun: expected one destination, got %d\n", len(args))
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.Run(ctx, cfg, app.StdIO()); err != nil {
		if errors.Is(err, context.Canceled) {
			return 0
		}
		fmt.Fprintln(os.Stderr, "autotun:", err)
		return 1
	}
	return 0
}

// runUpdate handles `autotun update`.
func runUpdate(args []string) int {
	var opts selfupdate.Options

	fs := pflag.NewFlagSet("autotun update", pflag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.SortFlags = false
	fs.BoolVar(&opts.CheckOnly, "check", false, "report whether an update is available, without installing it")
	fs.BoolVar(&opts.Force, "force", false, "install even if this build is not older than the release")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Replace this autotun with the latest release.\n\n"+
			"Usage:\n  autotun update [--check] [--force]\n\nFlags:\n")
		fmt.Fprintln(os.Stderr, fs.FlagUsages())
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "autotun update: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	opts.Current = buildinfo.Version()
	opts.Out = os.Stdout

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := selfupdate.Run(ctx, opts); err != nil {
		fmt.Fprintln(os.Stderr, "autotun update:", err)
		return 1
	}
	return 0
}
