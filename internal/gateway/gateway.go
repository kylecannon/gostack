package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
	"github.com/kylecannon/gostack/internal/route"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/tlscert"
)

const (
	// readHeaderTimeout bounds how long a client may take to send its request
	// headers, and to send its first byte. There is no write timeout: SQS
	// long-polls for 20 seconds and responses stream.
	readHeaderTimeout = 30 * time.Second

	// defaultDrainTimeout is how long Serve waits for requests in flight when
	// nothing planned the drain: DrainBy was never called and no PlanDrain
	// plan applied. A client holding a request open cannot stall a shutdown
	// longer than this.
	defaultDrainTimeout = 5 * time.Second

	// drainUnlimited is the drainBy value DrainBy stores for a zero deadline,
	// an explicit request to wait without limit (SHUTDOWN_TIMEOUT=0).
	drainUnlimited int64 = -1
)

const (
	phaseRunning int32 = iota
	phaseStopping
	phaseStopped
)

// Gateway serves AWS calls and the internal routes.
type Gateway struct {
	settings func() Settings
	regs     []service.Registration
	table    *route.Table
	eng      Dirtier

	cors         *corsRules
	corsEnforced *cors.Cors
	corsOpen     *cors.Cors

	// unnamed records, by registration, that markDirty has logged a rest call
	// that succeeded without naming its operation, so it logs only the first.
	unnamed []atomic.Bool

	phase   atomic.Int32
	handler http.Handler

	// tlsConfig is the certificate Listen loaded, for Serve.
	tlsConfig *tls.Config

	// drainBy is the Unix time in nanoseconds at which Serve stops waiting for
	// requests in flight. Zero means DrainBy was never called, and the drain
	// takes defaultDrainTimeout; drainUnlimited means no limit.
	drainBy atomic.Int64

	// planDrain, when set, is asked for drainBy when a listener fails.
	planDrain func(failedAt time.Time) time.Time
}

// New builds a gateway for the given registrations. settings is asked for on
// every request. internal mounts the internal routes (/_localstack and the
// like) and may be nil. eng may be nil when persistence is off.
//
// New fails when two registrations share a name, or one has no name or
// protocol, since the gateway could not tell their requests apart.
func New(
	settings func() Settings,
	regs []service.Registration,
	internal func(chi.Router),
	eng Dirtier,
) (*Gateway, error) {
	table, err := route.NewTable(regs)
	if err != nil {
		return nil, fmt.Errorf("build service table: %w", err)
	}

	g := &Gateway{settings: settings, regs: regs, table: table, eng: eng, unnamed: make([]atomic.Bool, len(regs))}
	g.cors = newCORSRules(settings())
	g.corsEnforced, g.corsOpen = newCORSHandlers(g.cors)
	g.handler = buildRouter(g, internal)

	return g, nil
}

// Handler returns the root router.
func (g *Gateway) Handler() http.Handler {
	return g.handler
}

// BeginShutdown makes the gateway refuse every request except internal calls
// and the developer paths, and then calls the Shutdown function of each
// registration that has one, so that work holding a request open, such as an
// SQS long poll, lets it go. Serve calls it when its context ends; the runtime
// calls it first when it has shutdown hooks to run. Only the first call does
// anything.
func (g *Gateway) BeginShutdown() {
	if !g.phase.CompareAndSwap(phaseRunning, phaseStopping) {
		return
	}

	for i := range g.regs {
		if shutdown := g.regs[i].Shutdown; shutdown != nil {
			shutdown()
		}
	}
}

// DrainBy sets the time at which Serve stops waiting for requests in flight and
// closes what is left. The caller plans the drain so that the shutdown hooks,
// the drain, the save of persisted state and the services' Close keep to one
// budget. A zero deadline is that plan's "no limit" (SHUTDOWN_TIMEOUT=0), and
// Serve then waits for the requests as long as they take. Without a call, Serve
// waits defaultDrainTimeout from the moment it starts to drain. Call it before
// Serve's context ends.
func (g *Gateway) DrainBy(deadline time.Time) {
	if deadline.IsZero() {
		g.drainBy.Store(drainUnlimited)

		return
	}

	g.drainBy.Store(deadline.UnixNano())
}

// PlanDrain sets plan, which Serve calls once, with the current time, when a
// listener fails and Serve starts the shutdown itself. Its result is what
// DrainBy takes. A shutdown that Serve's context asks for does not call it: the
// caller starts that one, runs the hooks, and sets DrainBy before the context
// ends. Call it before Serve.
func (g *Gateway) PlanDrain(plan func(failedAt time.Time) time.Time) {
	g.planDrain = plan
}

// buildRouter builds the root router of g. The middleware runs in the order of
// docs/architecture.md, which is the order of LocalStack's aws/app.py handler
// chain. CleanPath, StripSlashes, RedirectSlashes, Timeout and RequestID stay
// out: the first three rewrite S3 keys, Timeout would cut SQS long polls, and
// AWS request ids come from the protocol.
//
// Step 3 finds the service that claims a request by its host. Its request is
// served by the last middleware, inside the panic recovery and before the
// routes that services mount, so a claimed host wins over another service's
// paths as it does in LocalStack.
func buildRouter(g *Gateway, internal func(chi.Router)) http.Handler {
	r := chi.NewRouter()

	r.Use(markInternal)      // 1
	r.Use(g.shutdownGuard)   // 2
	r.Use(g.claimService)    // 3
	r.Use(g.preprocess)      // 4
	r.Use(g.corsMiddleware)  // 5
	r.Use(g.decodeContent)   // 6
	r.Use(g.responseHeaders) // 7
	r.Use(g.recoverPanic)    // 8
	r.Use(g.serveClaimed)    // 3, continued

	if internal != nil {
		internal(r)
	}

	for i := range g.regs {
		if g.regs[i].Mount != nil {
			g.regs[i].Mount(r)
		}
	}

	r.HandleFunc("/*", g.serveAWS)
	// chi registers "/*" for the methods it knows. Any other method token
	// reaches the services too, so that S3 answers a method no operation takes
	// with its own 405 instead of chi's empty one.
	r.MethodNotAllowed(g.serveAWS)

	return r
}

// Listen loads the TLS certificate and opens the addresses of Settings.Listen.
// The certificate comes first, so that one that cannot be used fails here, before
// the runtime has run a hook or printed Ready. The caller hands the result to
// Serve, which closes the listeners.
func (g *Gateway) Listen(ctx context.Context) ([]net.Listener, error) {
	tlsConfig, err := tlscert.Config(g.settings().CustomSSLCertPath)
	if err != nil {
		return nil, fmt.Errorf("tls certificate: %w", err)
	}

	g.tlsConfig = tlsConfig

	var (
		config    net.ListenConfig
		listeners []net.Listener
	)

	for _, address := range g.settings().Listen {
		listener, err := config.Listen(ctx, "tcp", address)
		if err != nil {
			for _, opened := range listeners {
				_ = opened.Close()
			}

			return nil, fmt.Errorf("listen on %s: %w", address, err)
		}

		listeners = append(listeners, listener)
	}

	return listeners, nil
}

// Serve serves the listeners until ctx ends or one of them fails, then shuts
// down: it refuses new work, waits for running requests until the time DrainBy
// set (defaultDrainTimeout from now when it was never called), and closes what
// is left. It returns nil after a shutdown that ctx asked for.
//
// Each listener takes HTTP/1.1, TLS (with h2) and cleartext HTTP/2 (h2c with
// prior knowledge) on the same port. Serve uses the certificate that Listen
// loaded, and loads it itself for listeners that did not come from Listen.
func (g *Gateway) Serve(ctx context.Context, listeners ...net.Listener) error {
	tlsConfig := g.tlsConfig
	if tlsConfig == nil {
		var err error

		tlsConfig, err = tlscert.Config(g.settings().CustomSSLCertPath)
		if err != nil {
			for _, listener := range listeners {
				_ = listener.Close()
			}

			return fmt.Errorf("tls certificate: %w", err)
		}
	}

	server := &http.Server{
		Handler:                      g.handler,
		ReadHeaderTimeout:            readHeaderTimeout,
		DisableGeneralOptionsHandler: true,
		Protocols:                    new(http.Protocols),
	}
	server.Protocols.SetHTTP1(true)
	server.Protocols.SetHTTP2(true)
	server.Protocols.SetUnencryptedHTTP2(true)

	failed := make(chan error, len(listeners))

	var serving sync.WaitGroup

	for _, listener := range listeners {
		mux := newMuxListener(listener, tlsConfig)

		serving.Go(func() {
			err := server.Serve(mux)
			if !errors.Is(err, http.ErrServerClosed) {
				failed <- err
			}
		})
	}

	var serveErr error

	select {
	case <-ctx.Done():
	case serveErr = <-failed:
		if g.planDrain != nil {
			g.DrainBy(g.planDrain(time.Now()))
		}
	}

	g.BeginShutdown()

	shutdownErr := g.shutdown(ctx, server)

	serving.Wait()
	g.phase.Store(phaseStopped)

	return errors.Join(serveErr, shutdownErr)
}

func (g *Gateway) shutdown(ctx context.Context, server *http.Server) error {
	// The caller's context has ended; the shutdown still gets its own time.
	ctx, cancel := g.drainContext(context.WithoutCancel(ctx))
	defer cancel()

	err := server.Shutdown(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		// Requests still running after the timeout, long polls for instance,
		// are cut. That is the shutdown working, not failing.
		err = server.Close()
	}

	if err != nil {
		return fmt.Errorf("shut down server: %w", err)
	}

	return nil
}

// drainContext limits ctx to the time DrainBy set, to defaultDrainTimeout from
// now when DrainBy was never called, and leaves it unlimited when DrainBy took a
// zero deadline.
func (g *Gateway) drainContext(ctx context.Context) (context.Context, context.CancelFunc) {
	switch nanos := g.drainBy.Load(); nanos {
	case 0:
		return context.WithTimeout(ctx, defaultDrainTimeout)
	case drainUnlimited:
		return ctx, func() {}
	default:
		return context.WithDeadline(ctx, time.Unix(0, nanos))
	}
}
