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
)

// call is what the recovery middleware, which sits outside service detection,
// learns about the request being served. serveAWS fills it in.
type call struct {
	rc *request.Context
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

// hooked wraps a seam in the middleware chain. The hook is read when a request
// arrives, so it can be set after the router is built.
func hooked(hook func() Hook) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if h := hook(); h != nil {
				if replaced := h(r); replaced != nil {
					r = replaced
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}

// decodeContent is root step 6: a gzip request body is decoded before anyone
// reads it. LocalStack skips S3 here because S3 does its own decoding; that
// skip arrives with S3.
func decodeContent(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") && r.Body != nil && r.Body != http.NoBody {
			r.Body = &gzipBody{src: r.Body}
			r.Header.Set("Content-Encoding", "identity")
			r.Header.Del("Content-Length")
			r.ContentLength = -1
		}

		next.ServeHTTP(w, r)
	})
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
}

func (w *statusWriter) WriteHeader(code int) {
	// A 1xx response is informational and does not end the header phase.
	if w.status == 0 && code >= http.StatusOK {
		w.status = code
	}

	w.ResponseWriter.WriteHeader(code)
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
