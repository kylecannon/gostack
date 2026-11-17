package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/kylecannon/gostack/internal/config"
	"github.com/kylecannon/gostack/internal/lifecycle"
)

// version is set at link time with -ldflags "-X main.version=<version>".
var version = "dev"

func main() {
	// The status command is the image HEALTHCHECK. It runs before anything that
	// loads the configuration, serves, or supervises.
	code, isStatus := statusMain(context.Background(), os.Args[1:], os.Stdout, os.Stderr)
	if isStatus {
		os.Exit(code)
	}

	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)

		return
	}

	os.Exit(run())
}

// run starts gostack and returns the exit code. As PID 1 it only supervises: it
// runs gostack again as a child, reaps zombies and restarts the child on
// request. Under any other PID it serves directly.
func run() int {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("gostack cannot start", "error", err)

		return 1
	}

	// The supervisor puts SUPERVISOR_PID in its child's environment. Only the
	// process that is not that child prints the entrypoint notices, so they
	// appear once per container.
	if os.Getenv(lifecycle.SupervisorPIDEnv) == "" {
		lifecycle.WriteStartupWarnings(os.Stdout, os.Getenv)
	}

	if lifecycle.NeedsSupervisor() {
		return supervise(cfg)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err = start(ctx, cfg, os.Stdout)
	if err != nil {
		slog.Error("gostack stopped", "error", err)

		return 1
	}

	return 0
}

// supervise runs the PID 1 supervisor until its child ends, and returns the
// child's exit code. The supervisor handles its own signals.
func supervise(cfg *config.Config) int {
	supervisor := &lifecycle.Supervisor{ShutdownTimeout: cfg.Settings().ShutdownTimeout}

	code, err := supervisor.Run(context.Background())
	if err != nil {
		slog.Error("supervisor stopped", "error", err)
	}

	return code
}
