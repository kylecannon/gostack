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
	decode DecodeHold
	heap   []func() // the releases of its heap reservations (KeepHeap)
	// stop is the channel whose close ends the request's waits on the pool
	// while it holds nothing of it (StopWaitsWith), or nil.
	stop <-chan struct{}
}

type chargeKey struct{}

// StopWaitsWith makes the close of stop end the request's waits on the
// in-flight bound while it holds nothing of it: its body queued for admission,
// or its decoding while it holds neither body bytes nor decode room. Each such
// wait fails with ErrBusy. A wait while it holds some of the bound goes on
// until its own context or the body wait ends it. The gateway hands every
// request the channel it closes as it begins to shut down, so a body it queued
// is answered with its service's throttle within the drain, not cut when the
// drain ends. Only a waiter reads it.
func (c *Charge) StopWaitsWith(stop <-chan struct{}) {
	c.stop = stop
}

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
	m.decode = &c.decode
	c.decode.body = m
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
	c.decode.body.Release()
}

// ReleaseDecode gives back what decoding holds. Decoding is over by then.
func (c *Charge) ReleaseDecode() {
	c.decode.Release()
}

// KeepHeap is HeapKeeper: release, a heap reservation's, runs when the request
// is released (Release).
func (c *Charge) KeepHeap(release func()) bool {
	c.heap = append(c.heap, release)

	return true
}

// ReleasePool is HeapKeeper: it gives back what the request's body and its
// decoding hold of the in-flight bound, and keeps the heap reservations until
// Release.
func (c *Charge) ReleasePool() {
	c.ReleaseBody()
	c.ReleaseDecode()
}

// PoolHeld is HeapKeeper: what the request's body and its decoding hold of
// the in-flight bound now.
func (c *Charge) PoolHeld() int64 {
	held := c.decode.Held()
	if c.decode.body != nil {
		held += c.decode.body.held
	}

	return held
}

// WaitContext is DenseWaiter: the request's context, or context.Background
// for a Charge that WithCharge did not put on one.
func (c *Charge) WaitContext() context.Context {
	if c.ctx == nil {
		return context.Background()
	}

	return c.ctx
}

// Release gives back everything the request holds. The gateway calls it when
// the handler returns.
func (c *Charge) Release() {
	c.ReleasePool()

	for _, release := range c.heap {
		release()
	}

	c.heap = nil
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
	if c := ChargeOf(ctx); c != nil && c.decode.body != nil {
		c.decode.body.armed = true
		if c.decode.body.held == 0 {
			c.decode.body.plan = plan
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
