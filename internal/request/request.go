package request

import (
	"context"
	"fmt"
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
	// DecodeFactor is the service's service.Registration.DecodeFactor: how many
	// bytes of decoded values each byte of the body may cost. Zero is
	// protocol.DecodeFactor.
	DecodeFactor int

	decode protocol.DecodeHold
}

// DecodeBudget is the most bytes decoding a body of n bytes may hold for this
// call's service: protocol.DecodeBudgetAt with DecodeFactor.
func (rc *Context) DecodeBudget(n int) int64 {
	return protocol.DecodeBudgetAt(n, rc.DecodeFactor)
}

// HoldDecode is protocol.DecodeHolder: it charges what decoding this call's
// body holds past protocol.FreeDecode against the in-flight bound, unless the
// call is internal (IsInternal). The charge is the request's protocol.Charge,
// which the gateway put on its context and gives back when the handler
// returns, whichever Context a service built for the call. A Context whose
// request carries none (a test, a benchmark) charges its own hold, which
// ReleaseDecode gives back.
func (rc *Context) HoldDecode(cost int64) (int64, error) {
	ctx := context.Background()
	if rc.Request != nil {
		ctx = rc.Request.Context()
	}

	var (
		covered int64
		err     error
	)

	c := protocol.ChargeOf(ctx)

	if IsInternal(ctx) {
		if c != nil {
			c.ExemptDecode()
		}

		rc.decode.Exempt()
	}

	if c != nil {
		covered, err = c.HoldDecode(cost)
	} else {
		covered, err = rc.decode.Cover(ctx, cost)
	}

	if err != nil {
		return 0, fmt.Errorf("hold %d bytes of decoding: %w", cost, err)
	}

	return covered, nil
}

// KeepHeap is protocol.HeapKeeper: the request's protocol.Charge keeps the
// heap reservation until the gateway releases it, when the handler returns. A
// Context whose request carries none does not take it: the decoder releases it
// once the body is decoded.
func (rc *Context) KeepHeap(release func()) bool {
	if c := rc.charge(); c != nil {
		return c.KeepHeap(release)
	}

	return false
}

// ReleasePool is protocol.HeapKeeper: it gives back what the request's body
// and its decoding hold of the in-flight bound, through its protocol.Charge,
// or what HoldDecode charged to this Context's own hold.
func (rc *Context) ReleasePool() {
	if c := rc.charge(); c != nil {
		c.ReleasePool()

		return
	}

	rc.decode.Release()
}

// PoolHeld is protocol.HeapKeeper: what ReleasePool would give back.
func (rc *Context) PoolHeld() int64 {
	if c := rc.charge(); c != nil {
		return c.PoolHeld()
	}

	return rc.decode.Held()
}

// ReleaseDecode gives back what HoldDecode charged to this Context's own hold,
// for a request that carries no protocol.Charge. What a request with one holds
// is given back by protocol.ReleaseBody, and by the gateway when the handler
// returns.
func (rc *Context) ReleaseDecode() {
	rc.decode.Release()
}

// charge is the protocol.Charge the gateway put on the request, or nil.
func (rc *Context) charge() *protocol.Charge {
	if rc.Request == nil {
		return nil
	}

	return protocol.ChargeOf(rc.Request.Context())
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
