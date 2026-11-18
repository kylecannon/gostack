package gateway

import (
	"context"
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
	// defaultShutdownTimeout is how long a graceful shutdown waits when the
	// settings name no timeout.
	defaultShutdownTimeout = 5 * time.Second
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

	dataPlane  Hook
	preprocess Hook

	phase   atomic.Int32
	handler http.Handler
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
	metadata := make([]service.Metadata, len(regs))
	for i := range regs {
		metadata[i] = regs[i].Metadata
	}

	table, err := route.NewTable(metadata)
	if err != nil {
		return nil, fmt.Errorf("build service table: %w", err)
	}

	g := &Gateway{settings: settings, regs: regs, table: table, eng: eng}
	g.cors = newCORSRules(settings())
	g.corsEnforced, g.corsOpen = newCORSHandlers(g.cors)
	g.handler = buildRouter(g, internal)

	return g, nil
}

// Handler returns the root router.
func (g *Gateway) Handler() http.Handler {
	return g.handler
}

// SetDataPlaneHook sets the seam at root step 3, where a request for a service
// addressed by its host (a bucket, a function URL, an API) is recognised. The
// first such service arrives after Phase 1. Call it before Serve.
func (g *Gateway) SetDataPlaneHook(h Hook) {
	g.dataPlane = h
}

// SetPreprocessHook sets the seam at root step 4, where a service can mark a
// request it handles itself before CORS and content decoding. Call it before
// Serve.
func (g *Gateway) SetPreprocessHook(h Hook) {
	g.preprocess = h
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

// buildRouter builds the root router of g. The middleware runs in the order of
// docs/architecture.md, which is the order of LocalStack's aws/app.py handler
// chain. CleanPath, StripSlashes, RedirectSlashes, Timeout and RequestID stay
// out: the first three rewrite S3 keys, Timeout would cut SQS long polls, and
// AWS request ids come from the protocol.
func buildRouter(g *Gateway, internal func(chi.Router)) http.Handler {
	r := chi.NewRouter()

	r.Use(markInternal)                                // 1
	r.Use(g.shutdownGuard)                             // 2
	r.Use(hooked(func() Hook { return g.dataPlane }))  // 3
	r.Use(hooked(func() Hook { return g.preprocess })) // 4
	r.Use(g.corsMiddleware)                            // 5
	r.Use(decodeContent)                               // 6
	r.Use(g.responseHeaders)                           // 7
	r.Use(g.recoverPanic)                              // 8

	if internal != nil {
		internal(r)
	}

	for i := range g.regs {
		if g.regs[i].Mount != nil {
			g.regs[i].Mount(r)
		}
	}

	r.HandleFunc("/*", g.serveAWS)

	return r
}

// Listen opens the addresses of Settings.Listen. The caller hands the result to
// Serve, which closes them.
func (g *Gateway) Listen(ctx context.Context) ([]net.Listener, error) {
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
// down: it refuses new work, waits up to Settings.ShutdownTimeout for running
// requests, and closes what is left. It returns nil after a shutdown that ctx
// asked for.
//
// Each listener takes HTTP/1.1, TLS (with h2) and cleartext HTTP/2 (h2c with
// prior knowledge) on the same port.
func (g *Gateway) Serve(ctx context.Context, listeners ...net.Listener) error {
	tlsConfig, err := tlscert.Config(g.settings().CustomSSLCertPath)
	if err != nil {
		for _, listener := range listeners {
			_ = listener.Close()
		}

		return fmt.Errorf("tls certificate: %w", err)
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
	}

	g.BeginShutdown()

	shutdownErr := g.shutdown(ctx, server)

	serving.Wait()
	g.phase.Store(phaseStopped)

	return errors.Join(serveErr, shutdownErr)
}

func (g *Gateway) shutdown(ctx context.Context, server *http.Server) error {
	timeout := g.settings().ShutdownTimeout
	if timeout <= 0 {
		timeout = defaultShutdownTimeout
	}

	// The caller's context has ended; the shutdown still gets its own time.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
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
