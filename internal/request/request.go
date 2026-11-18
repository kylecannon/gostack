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
