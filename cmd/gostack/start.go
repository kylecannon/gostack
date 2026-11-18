package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/kylecannon/gostack/api/openapi"
	"github.com/kylecannon/gostack/internal/config"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/internalapi"
	"github.com/kylecannon/gostack/internal/lifecycle"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service"
)

// start wires the packages together, serves until ctx ends or the process is
// killed through the internal API, and shuts down.
//
// The order is the contract's: the persisted state loads and the boot hooks run
// before the listeners open, the start and ready hooks run once they are open,
// and "Ready." follows the ready hooks.
func start(ctx context.Context, cfg *config.Config, stdout io.Writer) error {
	// Cancelling ctx is how the kill action stops the process.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The services report changes to the engine, and the engine is configured
	// from the services' names. Nothing reports a change before the engine is
	// set: constructors do not, and the state loads further down.
	var eng *persist.Engine

	services := newProviders(cfg, func(name string) { //nolint:contextcheck // the flush timer has no caller context
		eng.MarkDirty(name)
	})
	regs := services.regs
	eng = persist.New(cfg.PersistConfig(serviceNames(regs)...))
	services.persist(eng)

	// The engine is closed last, after the gateway has stopped and the services
	// have stopped their own goroutines, so that a request still running at
	// shutdown and a move task's final state are saved.
	defer closeEngine(ctx, eng, services)

	// A service whose state file is unsafe to touch has been logged and runs
	// without saving; the state routes and the health endpoint say so. It does not
	// stop the others from starting.
	err := eng.Load(ctx)
	if err != nil && !errors.Is(err, persist.ErrUnsafe) {
		return fmt.Errorf("load persisted state: %w", err)
	}

	hooks := lifecycle.NewHooks(lifecycle.DefaultInitRoot)
	proc := &lifecycle.Process{Hooks: hooks, Out: stdout}
	proc.Boot(ctx)

	// With persistence off the gateway gets no Dirtier at all: a nil *Engine
	// stored in the interface would not be nil.
	var dirtier gateway.Dirtier
	if eng.BaseDir() != "" {
		dirtier = eng
	}

	deps := internalapi.Deps{
		Version:       version,
		IsDocker:      cfg.Settings().InDocker,
		Ready:         proc.IsReady,
		Services:      &serviceTable{regs: regs, eng: eng},
		Init:          hooks,
		Config:        cfg,
		State:         &stateController{regs: regs, eng: eng},
		Restart:       restart,
		Kill:          cancel,
		ConfigUpdates: cfg.Settings().EnableConfigUpdates,
		CompatPaths:   cfg.Settings().CompatPaths,
		Debug:         cfg.Settings().Debug,
		OpenAPI:       openapi.Spec,
	}

	gw, err := gateway.New(
		settingsFunc(cfg),
		regs,
		func(r chi.Router) { internalapi.Mount(r, deps) },
		dirtier,
	)
	if err != nil {
		return fmt.Errorf("build gateway: %w", err)
	}

	listeners, err := gw.Listen(ctx)
	if err != nil {
		return err //nolint:wrapcheck // Listen names the address.
	}

	return serve(ctx, gw, listeners, proc, cfg.Settings().ShutdownTimeout)
}

// serve runs the gateway on listeners until ctx ends. The signal does not reach
// the gateway directly: the shutdown hooks run first, with the gateway already
// refusing AWS calls, and only then does the gateway drain its requests.
func serve(
	ctx context.Context,
	gw *gateway.Gateway,
	listeners []net.Listener,
	proc *lifecycle.Process,
	shutdownTimeout time.Duration,
) error {
	serveCtx, stopServing := context.WithCancel(context.WithoutCancel(ctx))
	defer stopServing()

	served := make(chan error, 1)

	go func() { served <- gw.Serve(serveCtx, listeners...) }()

	slog.Info("gostack listening", "addresses", addresses(listeners))
	proc.Start(ctx)
	proc.Ready(ctx)

	var err error

	select {
	case <-ctx.Done():
		shutDown(ctx, gw, proc, shutdownTimeout)
		stopServing()

		err = <-served
	case err = <-served:
		shutDown(ctx, gw, proc, shutdownTimeout)
	}

	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("serve: %w", err)
	}

	return nil
}

// shutDown makes the gateway refuse AWS calls, tells each service to let go of
// the requests it is holding (BeginShutdown does both), and runs the init
// shutdown hooks. The listeners are still open while it runs, so a call that
// arrives meanwhile is answered 503. The init hooks run even though ctx has
// ended, within SHUTDOWN_TIMEOUT (zero means no limit).
func shutDown(ctx context.Context, gw *gateway.Gateway, proc *lifecycle.Process, timeout time.Duration) {
	gw.BeginShutdown()

	ctx = context.WithoutCancel(ctx)
	if timeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	proc.Shutdown(ctx)
}

// closeEngine stops the services' own goroutines, and then writes the state that
// is still unsaved.
func closeEngine(ctx context.Context, eng *persist.Engine, services *providers) {
	services.close()

	err := eng.Close(context.WithoutCancel(ctx))
	if err != nil {
		slog.Error("save persisted state", "error", err)
	}
}

// restart asks the PID 1 supervisor to restart gostack. Without one there is
// nothing to ask.
func restart() {
	err := lifecycle.SignalRestart()

	switch {
	case errors.Is(err, lifecycle.ErrNoSupervisor):
		slog.Warn("restart requested, but gostack does not run under a supervisor")
	case err != nil:
		slog.Error("restart failed", "error", err)
	default:
	}
}

func serviceNames(regs []service.Registration) []string {
	names := make([]string, 0, len(regs))
	for i := range regs {
		names = append(names, regs[i].Metadata.Name)
	}

	return names
}

func addresses(listeners []net.Listener) []string {
	out := make([]string, 0, len(listeners))
	for _, listener := range listeners {
		out = append(out, listener.Addr().String())
	}

	return out
}
