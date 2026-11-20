package protocol

import (
	"context"
	"math"
)

// Charge is what one request holds against the in-flight bound (SetInFlight):
// its body, once a reader that buffers the body arms its meter (ArmBody), and
// what decoding the body holds past FreeDecode. The gateway puts one on the
// context of every request before any handler runs and releases it when the
// handler returns. Every reader and decoder finds it through the request's
// context, so a service that wraps the body or builds its own request.Context
// for a call charges the same Charge, and the gateway gives back all of it.
// A Charge is used on the request's own goroutine.
type Charge struct {
	ctx    context.Context //nolint:containedctx // the request's, for the waits while it is decoded
	meter  *BodyMeter
	decode DecodeHold
}

type chargeKey struct{}

// WithCharge returns ctx carrying c, for a request whose context is ctx. For a
// request that one service makes to another through the gateway (inproc), the
// caller then calls ExemptDecode: its caller's request was charged.
func WithCharge(ctx context.Context, c *Charge) context.Context {
	ctx = context.WithValue(ctx, chargeKey{}, c)
	c.ctx = ctx

	return ctx
}

// ChargeOf returns the Charge that WithCharge put on ctx, or nil.
func ChargeOf(ctx context.Context) *Charge {
	c, _ := ctx.Value(chargeKey{}).(*Charge)

	return c
}

// Meter makes m the request's body meter, so that ArmBody arms it and Release
// gives back what it holds. A body that states its length is admitted together
// with the room its decoding is planned to hold (BodyMeter.Read), which m
// charges to the request's decode hold.
func (c *Charge) Meter(m *BodyMeter) {
	c.meter = m
	m.decode = &c.decode
}

// HoldDecode is DecodeHolder: it makes the request's charge cover cost bytes of
// decoded values.
func (c *Charge) HoldDecode(cost int64) (int64, error) {
	if c.ctx == nil {
		return math.MaxInt64, nil
	}

	return c.decode.Cover(c.ctx, cost)
}

// ExemptDecode makes the request's decoding charge nothing, for an internal
// request (request.IsInternal).
func (c *Charge) ExemptDecode() {
	c.decode.Exempt()
}

// ReleaseBody gives back what the request's body holds and stops charging it.
func (c *Charge) ReleaseBody() {
	c.meter.Release()
}

// ReleaseDecode gives back what decoding holds. Decoding is over by then.
func (c *Charge) ReleaseDecode() {
	c.decode.Release()
}

// Release gives back everything the request holds. The gateway calls it when
// the handler returns.
func (c *Charge) Release() {
	c.ReleaseBody()
	c.ReleaseDecode()
}

// What decoding holds for each byte of an ordinary body, by the reader that
// arms the body (ArmBody, ReadRequestText): the room a body that states its
// length is admitted with. A body whose decoding holds more, one dense with
// elements, charges the rest as it decodes (DecodeHold.Cover).
const (
	// TextDecode is json's and xml's: json decodes a body twice, strict then
	// lenient, and charges both passes of its text (awsjson's decodePasses);
	// xml charges its character data twice (restxml's chargeToken).
	TextDecode = 2
	// FormDecode is query's: a form charges its pairs (query.PairCost), not its
	// text, and every legal form's pairs cost less than FreeDecode.
	FormDecode = 0
)

// ArmBody arms the body meter of the request whose context is ctx, for a reader
// that buffers or decodes the whole body as it reads it as json or xml
// (ReadRequest, rest.CapBody). A body that is streamed (an S3 object) is never
// armed, so it is never charged.
func ArmBody(ctx context.Context) {
	armBodyFor(ctx, TextDecode)
}

// armBodyFor is ArmBody for a reader whose decoding holds plan bytes for each byte
// of an ordinary body (TextDecode, FormDecode). A body already admitted keeps
// the plan it was admitted with.
func armBodyFor(ctx context.Context, plan int64) {
	if c := ChargeOf(ctx); c != nil && c.meter != nil {
		c.meter.armed = true
		if c.meter.held == 0 {
			c.meter.plan = plan
		}
	}
}

// ReleaseBody gives back all the request whose context is ctx holds, its body
// and its decoding, when the gateway put a Charge on it: a handler that holds
// its request open after it has decoded the body (an SQS long poll) calls it.
func ReleaseBody(ctx context.Context) {
	if c := ChargeOf(ctx); c != nil {
		c.Release()
	}
}
