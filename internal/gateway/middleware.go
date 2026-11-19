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
	"strings"

	"github.com/kylecannon/gostack/internal/awserr"
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
	rc *request.Context
}

// Record tells the recovery middleware which call r is, so that a panic is
// answered in the protocol of the service. The gateway does it for the calls it
// routes itself; a handler mounted by a service, which builds its own
// request.Context, does it with the request it was given.
func Record(r *http.Request, rc *request.Context) {
	if state, ok := r.Context().Value(callKey{}).(*call); ok {
		state.rc = rc
	}
}

// IsInternal reports whether ctx belongs to a call that a service made to
// another one, as marked by the x-localstack-data header.
func IsInternal(ctx context.Context) bool {
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
			if !IsInternal(r.Context()) && !isDeveloperPath(r.URL.Path) {
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
// LocalStack's ContentDecoder skips s3, which decodes its own.
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
// is no status left to send, so the connection is aborted.
func (g *Gateway) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := &call{}
		sw := &statusWriter{ResponseWriter: w}

		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}

			err, isError := recovered.(error)
			if isError && errors.Is(err, http.ErrAbortHandler) {
				panic(recovered)
			}

			g.answerPanic(sw, r, state.rc, recovered)
		}()

		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), callKey{}, state)))
	})
}

func (g *Gateway) answerPanic(w *statusWriter, r *http.Request, rc *request.Context, recovered any) {
	slog.ErrorContext(r.Context(), "panic while serving request",
		"method", r.Method, "path", r.URL.Path, "panic", fmt.Sprint(recovered), "stack", string(debug.Stack()))

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

	// pending is the call serveAWS is serving, whose success marks state dirty.
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
	p.g.markDirty(w, p.r, p.index, p.rc)
}
