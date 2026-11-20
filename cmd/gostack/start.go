package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/kylecannon/gostack/api/openapi"
	"github.com/kylecannon/gostack/internal/config"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/inproc"
	"github.com/kylecannon/gostack/internal/internalapi"
	"github.com/kylecannon/gostack/internal/lifecycle"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/service"
)

// callHandler is the handler the services' calls to each other go through: the
// gateway's own. A test replaces it to hold a call back until the shutdown has
// begun.
var callHandler = func(gw http.Handler) http.Handler { return gw }

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

	setRequestLimits(cfg.Settings())

	// The services report changes to the engine, and the engine is configured
	// from the services' names. Nothing reports a change before the engine is
	// set: constructors do not, and the state loads further down.
	var eng *persist.Engine

	services := newProviders(cfg, func(name string) { //nolint:contextcheck // the flush timer has no caller context
		eng.MarkDirty(name)
	})
	regs := services.regs
	eng = persist.New(cfg.PersistConfig(serviceNames(regs)...))

	// serve saves the state within the shutdown's deadline. The deferred call
	// covers a start that fails before serve runs, and does nothing after serve.
	saver := &saver{eng: eng, services: services}
	defer saver.saveUnlimited(ctx)

	err := loadState(ctx, services, eng)
	if err != nil {
		return err
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
		Protocol:      services.sns.MountProtocol,
		OpenAPI:       openapi.Spec,
	}

	gw, err := gateway.New(settingsFunc(cfg), regs, func(r chi.Router) { internalapi.Mount(r, deps) }, dirtier)
	if err != nil {
		return fmt.Errorf("build gateway: %w", err)
	}

	// The services call each other through a hub, so the shutdown can drain the
	// calls they queued before the gateway stops serving them.
	hub := inproc.NewHub(callHandler(gw.InternalHandler()))
	services.bind(hub)

	listeners, err := gw.Listen(ctx)
	if err != nil {
		return err //nolint:wrapcheck // Listen names the address.
	}

	return serve(ctx, gw, listeners, proc, cfg.Settings().ShutdownTimeout, hub,
		shutdownSave{first: saver.saveFirst, rest: saver.save})
}

// setRequestLimits sets the request body bounds once, before anything serves:
// GOSTACK_MAX_REQUEST_BODY, then GOSTACK_MAX_IN_FLIGHT_BODIES, which is raised
// to fit that limit, then GOSTACK_REQUEST_BODY_IDLE_TIMEOUT, how long those
// bodies may stall.
func setRequestLimits(s *config.Settings) {
	protocol.SetMaxBody(s.MaxRequestBody)
	protocol.SetInFlight(s.MaxInFlightBodies)
	protocol.SetBodyIdle(s.RequestBodyIdleTimeout)
}

// loadState hands eng the persisted services and loads their state. A service
// whose state file is unsafe to touch has been logged and runs without saving;
// the state routes and the health endpoint say so. It does not stop the others
// from starting. S3 takes its assets only once the load has accepted it.
func loadState(ctx context.Context, services *providers, eng *persist.Engine) error {
	assets, err := services.persist(eng)
	if err != nil {
		return err
	}

	err = eng.Load(ctx)
	if err != nil && !errors.Is(err, persist.ErrUnsafe) {
		return fmt.Errorf("load persisted state: %w", err)
	}

	services.useAssets(eng, assets)

	return nil
}

// serve runs the gateway on listeners until ctx ends, and shuts down within one
// budget, planned from shutdownTimeout (SHUTDOWN_TIMEOUT), that the supervisor's
// SIGKILL follows. The signal does not reach the gateway directly. The order is
// LocalStack's (runtime.py shutdown), with the save reserved ahead of everything
// that can overrun:
//
//  1. The gateway refuses AWS calls, and each service is told to let go of the
//     requests it holds, so that SQS wakes its long polls.
//  2. The init shutdown hooks run.
//  3. The gateway drains its requests in flight, and only to the time serve
//     gives it: the gateway has no timeout of its own. Its listeners close.
//  4. save.first writes the persisted state: the save gostack always made.
//  5. The calls the services queued for each other drain through calls, until
//     none is left or lifecycle.Deadlines.Calls, when the hub cuts the rest and
//     logs how many. The gateway has served them all along, as internal calls;
//     this waits for the ones the last requests queued. Then the gateway
//     refuses everything.
//  6. save.rest writes what the drain changed, and then stops the services.
//
// Steps 1 to 3 end at lifecycle.Deadlines.Work whatever they are doing. Both
// saves end at lifecycle.Deadlines.Save, and the services' Close at
// lifecycle.Deadlines.Close. A zero shutdownTimeout is no limit for all of
// them, and no calls drain at all: the calls still queued are cut at once,
// since nothing would end a call that queues itself again.
//
// The budget is planned once, from the moment the shutdown starts: the signal,
// or the failure of a listener. A failing listener makes the gateway drain by
// itself, before this function can act, so the gateway asks for the plan.
func serve(
	ctx context.Context,
	gw *gateway.Gateway,
	listeners []net.Listener,
	proc *lifecycle.Process,
	shutdownTimeout time.Duration,
	calls *inproc.Hub,
	save shutdownSave,
) error {
	serveCtx, stopServing := context.WithCancel(context.WithoutCancel(ctx))
	defer stopServing()

	// SHUTDOWN_TIMEOUT is read once: an update through /_localstack/config does not
	// move a deadline the supervisor already counts.
	budget := &shutdownBudget{timeout: shutdownTimeout}

	gw.PlanDrain(budget.drain)

	served := make(chan error, 1)

	go func() { served <- gw.Serve(serveCtx, listeners...) }()

	slog.Info("gostack listening", "addresses", addresses(listeners))
	proc.Start(ctx)
	proc.Ready(ctx)

	var (
		err      error
		deadline lifecycle.Deadlines
	)

	select {
	case <-ctx.Done():
		deadline = budget.plan(time.Now())
		gw.DrainBy(deadline.Work)

		shutDown(ctx, gw, proc, deadline.Work)
		stopServing()

		err = <-served
	case err = <-served:
		// The gateway stopped on its own and has drained, within the plan it asked
		// for. Only the hooks and the save are left, and they keep to that plan.
		deadline = budget.plan(time.Now())

		shutDown(ctx, gw, proc, deadline.Work)
	}

	saveAndClose(context.WithoutCancel(ctx), gw, calls, deadline, save)

	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("serve: %w", err)
	}

	return nil
}

// saveAndClose is serve's steps 4 to 6, once the requests have drained: the
// first save, the calls drain, the gateway's end, then the second save and the
// services' Close. ctx has no deadline of its own.
func saveAndClose(
	ctx context.Context, gw *gateway.Gateway, calls *inproc.Hub, deadline lifecycle.Deadlines, save shutdownSave,
) {
	saveCtx, cancelSave := lifecycle.Until(ctx, deadline.Save)
	defer cancelSave()

	closeCtx, cancelClose := lifecycle.Until(ctx, deadline.Close)
	defer cancelClose()

	if save.first != nil {
		save.first(saveCtx)
	}

	drainCalls(ctx, calls, deadline.Calls)
	gw.EndShutdown()

	save.rest(saveCtx, closeCtx)
}

// shutdownSave is the save serve makes, in two steps around the drain of the
// in-process calls. first may be nil.
type shutdownSave struct {
	// first writes the persisted state until saveCtx ends.
	first func(saveCtx context.Context)
	// rest writes what changed since, until saveCtx ends, and then stops the
	// services until closeCtx ends.
	rest func(saveCtx, closeCtx context.Context)
}

// drainCalls waits for the calls the services queued for each other until
// callsBy, and cuts the rest (inproc.Hub.Drain). A zero callsBy, no limit, cuts
// at once. hub may be nil.
func drainCalls(ctx context.Context, hub *inproc.Hub, callsBy time.Time) {
	if hub == nil {
		return
	}

	var cancel context.CancelFunc

	if callsBy.IsZero() {
		ctx, cancel = context.WithCancel(ctx)
		cancel()
	} else {
		ctx, cancel = context.WithDeadline(ctx, callsBy)
		defer cancel()
	}

	hub.Drain(ctx)
}

// shutdownBudget plans a shutdown's deadlines once, for the moment the shutdown
// starts, and gives the same plan to every later caller. The shutdown starts at
// the signal, or at the failure of a listener, and no shutdown spends the
// timeout twice.
type shutdownBudget struct {
	timeout time.Duration

	mu      sync.Mutex
	planned bool
	set     lifecycle.Deadlines
}

// drain is the plan's Work, for gateway.PlanDrain.
func (b *shutdownBudget) drain(failedAt time.Time) time.Time {
	return b.plan(failedAt).Work
}

// plan returns the deadlines, planned from at on the first call.
func (b *shutdownBudget) plan(at time.Time) lifecycle.Deadlines {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.planned {
		b.set, b.planned = lifecycle.PlanShutdown(at, b.timeout), true
	}

	return b.set
}

// shutDown makes the gateway refuse AWS calls, tells each service to let go of
// the requests it is holding (BeginShutdown does both), and runs the init
// shutdown hooks, until workBy (zero means no limit). The listeners are still
// open while it runs, so a call that arrives meanwhile is answered 503. The
// hooks run even though ctx has ended.
//
// A service's shutdown function cannot be cancelled, so the two run apart from
// the caller: if they overrun workBy the caller goes on to drain and save, and
// the gateway is refusing calls already.
func shutDown(ctx context.Context, gw *gateway.Gateway, proc *lifecycle.Process, workBy time.Time) {
	ctx, cancel := lifecycle.Until(context.WithoutCancel(ctx), workBy)
	defer cancel()

	done := make(chan struct{})

	go func() {
		defer close(done)

		gw.BeginShutdown()
		proc.Shutdown(ctx)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		slog.Warn("shutdown hooks did not finish in time; draining and saving without them")
	}
}

// saver writes the persisted state when gostack stops, once.
type saver struct {
	eng      *persist.Engine
	services *providers
	first    sync.Once
	rest     sync.Once
}

// saveFirst writes the state that changed, until ctx ends: the save that runs
// once the requests have drained, before the in-process calls drain, so that a
// slow drain never costs what was acknowledged before the shutdown. It is the
// engine's Stop, which also stops the timer and leaves the assets open for the
// saves that follow. A write cut short keeps the old file.
func (s *saver) saveFirst(ctx context.Context) {
	s.first.Do(func() {
		err := s.eng.Stop(ctx)
		if err != nil {
			slog.Error("save persisted state", "error", err)
		}
	})
}

// save makes saveFirst when it has not run, and then writes what changed since,
// such as the calls the drain delivered, until saveCtx ends: when that second
// write cannot finish, the state of the first stands. Then it stops the
// services' own goroutines, all at once, under a budget of their own. Every
// Close is given closeCtx and none waits for another; one that has not returned
// when closeCtx ends is abandoned, with a warning, and never takes time from
// the writes. A Close that returned at the deadline, because it honours its
// context, is not abandoned.
//
// What the services' Close changes, and what a request still running at the
// end of the drain changes, is written by a last flush, when every Close
// returned and closeCtx has time left for it. That flush is the engine's
// Close, which then releases the assets' descriptors; the first save is its
// Stop, which only stops the timer, and the second its Flush, so the last flush
// can still sync and remove assets and no service's Close finds them released.
// The calls stay as they are: Test_saver_save_order fails when Stop or Close is
// swapped.
func (s *saver) save(saveCtx, closeCtx context.Context) {
	s.rest.Do(func() {
		s.saveFirst(saveCtx)

		// A first save that ran out of time has said so; this one would only
		// fail the same way.
		if saveCtx.Err() == nil {
			err := s.eng.Flush(saveCtx)
			if err != nil {
				slog.Error("save persisted state", "error", err)
			}
		}

		if !s.closeServices(closeCtx) {
			return
		}

		err := s.eng.Close(closeCtx)
		if err != nil {
			slog.Error("save persisted state", "error", err)
		}
	})
}

// saveUnlimited is save with no deadline, for a start that fails before serve
// runs.
func (s *saver) saveUnlimited(ctx context.Context) {
	unlimited := context.WithoutCancel(ctx)

	s.save(unlimited, unlimited)
}

// closeServices runs the services' Close functions, all at once, until ctx
// ends. It warns of each whose Close had not returned by then, and reports
// whether a last save can follow: every Close returned, and ctx still has time
// for it. A Close that returned at the deadline, because it honours ctx, is not
// warned of as running, but the last save its return skips is warned of, once,
// since what the Close changed stays unsaved.
func (s *saver) closeServices(ctx context.Context) bool {
	running := s.services.close(ctx)
	if len(running) > 0 {
		slog.Warn("a service did not stop in time; exiting without waiting for it", "services", running)

		return false
	}

	if ctx.Err() != nil {
		slog.Warn("the last save was skipped because the close deadline passed; changes made in Close are not saved")

		return false
	}

	return true
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
