package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/kylecannon/gostack/internal/gateway"
)

// version is set at link time with -ldflags "-X main.version=<version>".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)

		return
	}

	os.Exit(run())
}

// run starts gostack and returns the exit code.
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := start(ctx, os.Stdout)
	if err != nil {
		slog.Error("gostack stopped", "error", err)

		return 1
	}

	return 0
}

// start wires the packages together, serves until ctx ends and shuts down.
//
// The lines marked "Integrator step" stand in for packages that other tickets
// add. The integrator replaces each one with the real call and deletes its stub
// in stubs.go.
func start(ctx context.Context, stdout io.Writer) error {
	settings := loadSettings() // Integrator step: config.Load, then read each setting from *config.Config.

	// Integrator step: persist.New(cfg), Register each service's Snapshotter, Load.
	// The engine goes in here only when persistence is on. With it off, leave eng
	// as the nil interface: a nil *persist.Engine stored in a Dirtier is not nil.
	var eng gateway.Dirtier

	gw, err := gateway.New(
		func() gateway.Settings { return settings },
		registrations(),
		mountInternal, // Integrator step: internalapi.Mount.
		eng,
	)
	if err != nil {
		return fmt.Errorf("build gateway: %w", err)
	}

	listeners, err := gw.Listen(ctx)
	if err != nil {
		return err //nolint:wrapcheck // Listen names the address.
	}

	return serve(ctx, gw, listeners, stdout)
}

// serve runs the gateway on listeners until ctx ends.
//
// Integrator step: internal/lifecycle takes this over: boot.d hooks before
// the listeners open, start.d and ready.d after, the "Ready." marker once the
// ready hooks are done, shutdown.d hooks on the way out, and the PID 1
// supervisor. lifecycle should call gw.BeginShutdown before the shutdown
// hooks run, and eng.Flush after the gateway has stopped.
func serve(ctx context.Context, gw *gateway.Gateway, listeners []net.Listener, stdout io.Writer) error {
	slog.Info("gostack listening", "addresses", addresses(listeners))
	_, _ = fmt.Fprintln(stdout, "Ready.")

	err := gw.Serve(ctx, listeners...)
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("serve: %w", err)
	}

	return nil
}

func addresses(listeners []net.Listener) []string {
	out := make([]string, 0, len(listeners))
	for _, listener := range listeners {
		out = append(out, listener.Addr().String())
	}

	return out
}
