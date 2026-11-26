package gateway

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/route"
	"github.com/kylecannon/gostack/internal/service"
)

// internalRequestHeader carries the parameters of a call that one service makes
// to another, as json. Its presence marks the call as internal.
const (
	internalRequestHeader = "X-Localstack-Data"
	valueTrue             = "true"
)

type (
	internalKey struct{}
	callKey     struct{}
	claimKey    struct{}
)

// claim is the service that root step 3 found for a request by its host, or
// why that service cannot take it.
type claim struct {
	match route.Match
	err   error
}

// call is what the recovery middleware, which sits outside service detection,
// learns about the request being served. serveAWS fills it in.
type call struct {
	rc     *request.Context
	charge protocol.Charge
}

// Record tells the recovery middleware which call r is, so that a panic is
// answered in the protocol of the service. The gateway does it for the calls it
// routes itself; a handler mounted by a service, which builds its own
// request.Context, does it with the request it was given. On a mounted route
// it also decides the dirty mark of a handler that panics or calls
// runtime.Goexit: the call is judged by the operation Record names, and a
// route that never calls Record is not marked.
func Record(r *http.Request, rc *request.Context) {
	if state, ok := r.Context().Value(callKey{}).(*call); ok {
		state.rc = rc
	}
}

// ServedAsInternal reports whether the gateway serves ctx's call as one a
// service made to another while it is stopping: one served through
// InternalHandler, which nothing outside the process can reach, or one marked
// by the x-localstack-data header. Any client can send the header, so this is
// never grounds to exempt a request from a memory bound; request.IsInternal,
// which no header sets, is.
func ServedAsInternal(ctx context.Context) bool {
	internal, _ := ctx.Value(internalKey{}).(bool)

	return internal
}

// markInternal is root step 1. A header that does not hold a json object is
// ignored, as LocalStack ignores a parameter blob it cannot load.
func markInternal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if value := r.Header.Get(internalRequestHeader); value != "" {
			var params map[string]any

			err := json.Unmarshal([]byte(value), &params)
			if readable := err == nil && params != nil; readable {
				r = r.WithContext(context.WithValue(r.Context(), internalKey{}, true))
			} else {
				slog.WarnContext(r.Context(), "ignoring unreadable internal request parameters")
			}
		}

		next.ServeHTTP(w, r)
	})
}

// shutdownGuard is root step 2, LocalStack's RuntimeShutdownHandler. Once the
// gateway has stopped it answers 503. While it is stopping, it still serves
// internal calls and the developer paths.
func (g *Gateway) shutdownGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch g.phase.Load() {
		case phaseStopped:
			g.refuse(w)

			return
		case phaseStopping:
			if !ServedAsInternal(r.Context()) && !isDeveloperPath(r.URL.Path) {
				g.refuse(w)

				return
			}
		default:
			// Running.
		}

		next.ServeHTTP(w, r)
	})
}

func isDeveloperPath(path string) bool {
	return strings.HasPrefix(path, "/_localstack") || strings.HasPrefix(path, "/_gostack")
}

func (g *Gateway) refuse(w http.ResponseWriter) {
	g.identify(w.Header())
	w.WriteHeader(http.StatusServiceUnavailable)
}

// claimService is root step 3, LocalStack's ServiceLoaderForDataPlane: a
// service that knows a request by its host alone, through
// Registration.ClaimHost, takes it before any other detection. The claim rides
// on the request until serveClaimed hands it to serveAWS.
func (g *Gateway) claimService(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		match, err := g.table.Claim(r)
		if !errors.Is(err, route.ErrNoService) {
			r = r.WithContext(context.WithValue(r.Context(), claimKey{}, &claim{match: match, err: err}))
		}

		next.ServeHTTP(w, r)
	})
}

// claimOf returns the claim root step 3 put on r.
func claimOf(r *http.Request) (*claim, bool) {
	c, ok := r.Context().Value(claimKey{}).(*claim)

	return c, ok
}

// serveClaimed is the last middleware: it serves a claimed request as an AWS
// call before the router can give it to a route another service mounts. The
// internal paths keep their routes, which LocalStack also serves after its
// data-plane step.
func (g *Gateway) serveClaimed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := claimOf(r); ok && !isInternalPath(r.URL.Path) {
			g.serveAWS(w, r)

			return
		}

		next.ServeHTTP(w, r)
	})
}

// isInternalPath reports whether path is one of gostack's own, which no claim
// takes.
func isInternalPath(path string) bool {
	return isDeveloperPath(path) || strings.HasPrefix(path, "/_aws")
}

// preprocess is root step 4, LocalStack's preprocess_request: the Preprocess
// of each registration, in registration order, sees every request before CORS
// and content decoding, and may replace it.
//
// A request that step 3 claimed by its host belongs to the claiming service.
// The gateway marks it with service.Own for the claimant, so its RawBody,
// OwnCORS and route detection apply without a Preprocess to say so, and no
// other service's Preprocess sees it: a mark from one would take the request
// from its claimant, whose gzip decoding and CORS it would skip. The claimant's
// own Preprocess still runs, as it is how S3 rewrites a virtual-host request.
// A claim that is an error (served as one) has no claimant, so nothing is
// marked and no Preprocess runs, and the gateway's own paths, which no claim
// takes, are not marked.
func (g *Gateway) preprocess(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claimed, isClaimed := claimOf(r)

		if isClaimed && claimed.err == nil && !isInternalPath(r.URL.Path) {
			r = service.Own(r, g.regs[claimed.match.Index].Metadata.Name)
		}

		for i := range g.regs {
			if isClaimed && (claimed.err != nil || claimed.match.Index != i) {
				continue
			}

			if preprocess := g.regs[i].Preprocess; preprocess != nil {
				if replaced := preprocess(r); replaced != nil {
					r = replaced
				}
			}
		}

		next.ServeHTTP(w, r)
	})
}

// owner returns the Registration of the service a Preprocess marked r for
// (service.Own), or nil.
func (g *Gateway) owner(r *http.Request) *service.Registration {
	name := service.OwnerOf(r)
	if name == "" {
		return nil
	}

	for i := range g.regs {
		if g.regs[i].Metadata.Name == name {
			return &g.regs[i]
		}
	}

	return nil
}

// decodeContent is root step 6: a gzip request body is decoded before anyone
// reads it, unless the request's owner sets Registration.RawBody, as
// LocalStack's ContentDecoder skips s3, which decodes its own. A body from a
// listener is then metered against the in-flight bound (meterBody, at step 8)
// until the handler returns.
func (g *Gateway) decodeContent(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") && r.Body != nil && r.Body != http.NoBody &&
			!g.rawBody(r) {
			r.Body = &gzipBody{src: r.Body}
			r.Header.Set("Content-Encoding", "identity")
			r.Header.Del("Content-Length")
			r.ContentLength = -1
		}

		next.ServeHTTP(w, r)
	})
}

// meterBody meters the body of a request from a listener against the in-flight
// bound (protocol.SetInFlight), after any gzip decoding, so the bound counts
// decoded bytes. A body that states FreeBody bytes or fewer is never metered,
// so a small request takes no lock and no atomic here. It returns nil when the
// body is not metered or there is no bound.
func meterBody(r *http.Request) *protocol.BodyMeter {
	if r.Body == nil || r.Body == http.NoBody ||
		(r.ContentLength >= 0 && r.ContentLength <= protocol.FreeBody) {
		return nil
	}

	return protocol.NewBodyMeter(r.Context(), r.Body, r.ContentLength)
}

// rawBody reports whether the owner of r reads its body as sent.
func (g *Gateway) rawBody(r *http.Request) bool {
	owner := g.owner(r)

	return owner != nil && owner.RawBody
}

// gzipBody decodes gzip when it is first read, so a body that is not gzip fails
// the read that needs it, not the request.
type gzipBody struct {
	src io.ReadCloser
	zr  *gzip.Reader
	err error
}

func (b *gzipBody) Read(p []byte) (int, error) {
	if b.zr == nil && b.err == nil {
		b.zr, b.err = gzip.NewReader(b.src)
	}

	if b.err != nil {
		return 0, b.err
	}

	return b.zr.Read(p) //nolint:wrapcheck // io.Reader: the error of the Read it delegates to.
}

func (b *gzipBody) Close() error {
	if b.zr != nil {
		_ = b.zr.Close()
	}

	return b.src.Close()
}

// responseHeaders is root step 7. It sets x-localstack and x-gostack before the
// handler runs, so every response carries them, whoever writes it.
func (g *Gateway) responseHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.identify(w.Header())
		next.ServeHTTP(w, r)
	})
}

// identify adds the identification headers when the settings ask for them. The
// keys are lower case, as LocalStack sends them.
func (g *Gateway) identify(header http.Header) {
	if !g.settings().ResponseHeaderEnabled {
		return
	}

	header["x-localstack"] = []string{valueTrue}
	header["x-gostack"] = []string{valueTrue}
}

// recoverPanic is root step 8, the innermost. A panic in a handler becomes a 500
// InternalError in the protocol of the service being called, or a 501 under
// FAIL_FAST, and the process keeps running. Once the response has started there
// is no status left to send, so the connection is aborted. A handler that
// panics with http.ErrAbortHandler aborts the connection itself.
//
// Every panic is logged, http.ErrAbortHandler too. A call whose handler did not
// return, because it panicked or called runtime.Goexit, is marked dirty when its
// operation changes state, whatever status it wrote: the handler may have made
// its change first. That holds on a route a service mounts too, which marks its
// own changes only once its handler returns. The mark comes before the log, so
// a log that fails cannot lose it.
//
// It also puts the request's protocol.Charge on its context, with the body's
// meter (meterBody), and gives back all it holds when the handler ends, by
// returning, panicking or calling runtime.Goexit, whatever the handler did with
// the body or built as its request.Context.
func (g *Gateway) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := &call{}
		sw := &statusWriter{ResponseWriter: w}
		returned := false

		ctx := protocol.StopWaitsWith(context.WithValue(r.Context(), callKey{}, state), g.stopping)
		r = r.WithContext(protocol.WithCharge(ctx, &state.charge))

		// A request that one service makes to another through the gateway
		// (inproc) carries request.MarkInternal, which no header or listener
		// can set: its body and decoding were charged with the request that
		// caused it.
		if request.IsInternal(r.Context()) {
			state.charge.ExemptDecode()
		} else if meter := meterBody(r); meter != nil {
			meter.IdleBy(http.NewResponseController(w).SetReadDeadline)
			state.charge.Meter(meter)
			r.Body = meter
		}

		defer state.charge.Release()

		defer func() {
			if returned {
				return
			}

			// recover is nil for a handler that called runtime.Goexit, which
			// has no answer to send and goes on ending its goroutine.
			g.recoverAbandoned(sw, r, state.rc, recover())
		}()

		next.ServeHTTP(sw, r)

		returned = true
	})
}

// recoverAbandoned settles a call whose handler did not return: it marks the
// call dirty, then, for a panic, logs it and answers it.
func (g *Gateway) recoverAbandoned(sw *statusWriter, r *http.Request, rc *request.Context, recovered any) {
	g.markAbandoned(sw, rc)

	if recovered == nil {
		return
	}

	logPanic(r, rc, recovered)

	err, isError := recovered.(error)
	if isError && errors.Is(err, http.ErrAbortHandler) {
		panic(recovered)
	}

	g.answerPanic(sw, rc, recovered)
}

// markAbandoned marks dirty a call whose handler did not return, when its
// operation changes state, whatever status it wrote, unless the call has been
// marked already. A call serveAWS serves is judged as the call serveAWS gave
// it, whatever its Serve later named with Record. A call on a route a service
// mounts, where serveAWS armed nothing, is judged by the registration of the
// service rc names; with no rc, it is not marked.
func (g *Gateway) markAbandoned(sw *statusWriter, rc *request.Context) {
	p := &sw.pending
	if p.marked {
		return
	}

	index := p.index
	if p.rc != nil {
		rc = p.rc
	} else {
		if rc == nil {
			return
		}

		index = slices.IndexFunc(g.regs, func(reg service.Registration) bool { return reg.Metadata.Name == rc.Service })
		if index < 0 {
			return
		}
	}

	p.marked = g.markChanged(index, rc)
}

// logPanic logs a handler's panic with the service and operation of the call,
// when the gateway has learned them.
func logPanic(r *http.Request, rc *request.Context, recovered any) {
	attrs := []any{"method", r.Method, "path", r.URL.Path}

	if rc != nil {
		attrs = append(attrs, "service", rc.Service)

		if rc.Operation != "" {
			attrs = append(attrs, "operation", rc.Operation)
		}
	}

	attrs = append(attrs, "panic", fmt.Sprint(recovered), "stack", string(debug.Stack()))

	slog.ErrorContext(r.Context(), "panic while serving request", attrs...)
}

func (g *Gateway) answerPanic(w *statusWriter, rc *request.Context, recovered any) {
	if w.status != 0 {
		panic(http.ErrAbortHandler)
	}

	status := http.StatusInternalServerError
	if g.settings().FailFast {
		status = http.StatusNotImplemented
	}

	if rc == nil {
		writeFailure(w, http.StatusInternalServerError, failure{
			Error:   "Unexpected exception",
			Message: fmt.Sprint(recovered),
			Type:    fmt.Sprintf("%T", recovered),
		})

		return
	}

	message := fmt.Sprintf("exception while calling %s with unknown operation: %v", rc.Service, recovered)
	if rc.Operation != "" {
		message = fmt.Sprintf("exception while calling %s.%s: %v", rc.Service, rc.Operation, recovered)
	}

	failure := awserr.Internal(message)
	failure.StatusCode = status

	writeAWSError(w, rc, failure)
}

// statusWriter records the status code, so the gateway can tell a success from
// an error after a Registration has written its own response.
type statusWriter struct {
	http.ResponseWriter

	status int

	// pending is the call serveAWS is serving, whose success marks state dirty,
	// and whether it has been marked.
	pending pendingMark
}

// pendingMark holds what markDirty needs to judge one call, so that the mark
// can run before the call's response reaches the client. A value field of
// statusWriter, so holding it costs no allocation.
type pendingMark struct {
	g     *Gateway
	r     *http.Request
	rc    *request.Context
	index int
	armed bool
	// marked records that the call marked state dirty, so that a handler
	// that goes on to panic is not marked twice.
	marked bool
}

func (w *statusWriter) WriteHeader(code int) {
	// A 1xx response is informational and does not end the header phase.
	if w.status == 0 && code >= http.StatusOK {
		w.status = code
	}

	w.ResponseWriter.WriteHeader(code)
}

// NoteStatus records the status of a response the service writes through a
// connection it took with Hijack, which never reaches WriteHeader. Without it a
// 4xx on such a response reads as 0, counts as a success and marks state dirty,
// and a panic after the response began would write a 500 into the closed
// connection. A service finds it through Unwrap, by this method alone.
//
// It also marks state dirty for a successful write, here rather than after the
// service returns: the service sends the response itself and can finish it
// before it returns, and a client that reads the response and asks for a save
// at once must find the change marked. A service calls it before it writes a
// byte, once the call has changed what it changes.
func (w *statusWriter) NoteStatus(code int) {
	if w.status == 0 && code >= http.StatusOK {
		w.status = code
	}

	if w.status != 0 {
		w.settle()
	}
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}

	return w.ResponseWriter.Write(p) //nolint:wrapcheck // io.Writer: the error of the Write it delegates to.
}

// Flush sends buffered data to the client, which streaming responses need.
func (w *statusWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}

	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// succeeded reports whether the response is not an error. A handler that wrote
// nothing sends 200.
func (w *statusWriter) succeeded() bool {
	return w.status < http.StatusBadRequest
}

// settle marks state dirty for the pending call, once: the first call does it
// and later ones do nothing.
func (w *statusWriter) settle() {
	p := &w.pending
	if !p.armed {
		return
	}

	p.armed = false
	p.marked = p.g.markDirty(w, p.r, p.index, p.rc)
}
