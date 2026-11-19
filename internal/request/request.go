package request

import (
	"context"
	"net/http"

	"github.com/kylecannon/gostack/internal/protocol"
)

// Context is the gateway's parse of one AWS call.
type Context struct {
	// Service is the botocore service name, for example "sqs" or "sts".
	Service string
	// Operation is the operation name, for example "SendMessage".
	Operation string
	// Protocol is the wire protocol the caller used.
	Protocol protocol.Kind
	// AccountID is the account the call acts on.
	AccountID string
	// Region is the region the call acts on.
	Region string
	// RequestID is a UUIDv4 that the response echoes.
	RequestID string
	// Request is the inbound HTTP request.
	Request *http.Request
	// CORS is the gateway's own CORS policy, set only when the request's
	// owner handles CORS itself (Registration.OwnCORS, DISABLE_CUSTOM_CORS_*
	// not set for it). It is nil when the gateway's CORS has answered the
	// request already.
	CORS DefaultCORS
}

// DefaultCORS is the gateway's CORS policy, which a service that handles CORS
// itself falls back to for the requests its own rules do not cover, as
// LocalStack's S3 CORS handler calls CorsEnforcer and CorsResponseEnricher.
type DefaultCORS interface {
	// OriginAllowed is CorsEnforcer.is_cors_origin_allowed: whether the
	// Origin, or else the Referer, is one the gateway allows. A request with
	// neither is allowed.
	OriginAllowed(header http.Header) bool
	// ChecksDisabled is DISABLE_CORS_CHECKS.
	ChecksDisabled() bool
	// AddHeaders is CorsResponseEnricher.add_cors_headers: it adds the
	// Access-Control-* headers the gateway sends to a request with an Origin,
	// except the ones response already holds.
	AddHeaders(response http.Header, r *http.Request)
}

type contextKey struct{}

// From returns the Context that With stored in ctx. The second result is false
// when ctx carries none.
func From(ctx context.Context) (*Context, bool) {
	rc, ok := ctx.Value(contextKey{}).(*Context)

	return rc, ok
}

// With returns a copy of ctx that carries rc.
func With(ctx context.Context, rc *Context) context.Context {
	return context.WithValue(ctx, contextKey{}, rc)
}
