package protocol

import (
	"context"
	"fmt"
	"math"
	"runtime/debug"
)

const (
	// FreeDecode is what decoding one request may hold before what it holds is
	// charged against the in-flight bound (SetInFlight). Every ordinary request
	// decodes into less, so it takes no lock and no atomic for it.
	FreeDecode = 1 << 20

	// decodeChunk is the step a decoding's charge grows by past FreeDecode.
	decodeChunk = 1 << 20
)

// ErrOverMemory is what HoldDecode fails with when decoding would cost more than
// the process's memory limit can hold (overMemory). It is an ErrBusy for a
// caller that does not tell them apart, and fails every later call too. A json
// service answers it as a body that decodes into more than gostack allows, not
// as a throttle, since waiting does not make room for it.
var ErrOverMemory = fmt.Errorf("%w: decoding the body would not fit the memory limit", ErrBusy)

const (
	// costPerHeapByte is how many bytes of decode cost one byte of the heap
	// stands for at most. The cost model charges two passes and append's
	// regrowth: a list of {"N":"1"} is charged 1,638 bytes an element and keeps
	// about 216 live, 7.6 to 1, and a Scan holding its parameter twice about
	// 4 to 1.
	costPerHeapByte = 4

	// heapShare is the part of the memory limit, as a divisor, that what one
	// decoding keeps live may take, leaving the rest to the runtime, the
	// services' state and the garbage a collection has not yet found.
	heapShare = 2
)

// overMemory reports whether decoding at cost would keep more of the heap
// than heapShare of the Go memory limit allows, as the limit is now
// (debug.SetMemoryLimit, which cmd/gostack sets from a container's). With no
// limit nothing is over it.
func overMemory(cost int64) bool {
	limit := debug.SetMemoryLimit(-1)

	return limit < math.MaxInt64 && cost/costPerHeapByte > limit/heapShare
}

// DecodeHolder is what a decoder charges what it holds to as it decodes a
// request's body: request.Context, through a DecodeHold. A decoder calls
// HoldDecode only once what it holds passes what the last call returned
// (FreeDecode before the first).
type DecodeHolder interface {
	// HoldDecode makes the request's charge cover cost bytes of decoded
	// values and returns what it covers now. It fails with ErrBusy when the
	// in-flight bound leaves no room before the request's wait ends, and with
	// the same error on every later call.
	HoldDecode(cost int64) (covered int64, err error)
}

// DecodeHold is what decoding one request holds against the in-flight bound
// past FreeDecode, charged in steps of decodeChunk from the same pool as the
// request bodies and waiting in the same queue, ahead of bodies that hold
// nothing yet. When it has to wait for more than it holds, it gives back what
// it holds and waits for all it needs (bodyPool.regrow). Its zero value holds
// nothing. It is used on the request's own goroutine.
type DecodeHold struct {
	pool *bodyPool
	held int64
	room int64 // the most it charges: the bound less one body at its cap
	free bool  // charges nothing: no bound, an internal request, or released
	err  error
}

// Cover makes the hold cover cost bytes of decoding for a request whose
// context is ctx, waiting for room as a body does, and returns what it covers.
// One request's charge stops at the bound less one body's share (bodyShare),
// at least as much as that share, so a request alone always fits whatever the
// bound and the service's decode factor; what it decodes beyond that is not
// charged.
func (h *DecodeHold) Cover(ctx context.Context, cost int64) (int64, error) {
	switch {
	case h.err != nil:
		return 0, h.err
	case h.free:
		return math.MaxInt64, nil
	case cost <= FreeDecode+h.held:
		return FreeDecode + h.held, nil
	}

	if h.pool == nil {
		pool := inFlight.Load()
		if pool == nil {
			h.free = true

			return math.MaxInt64, nil
		}

		h.pool, h.room = pool, pool.capacity-pool.bodyShare()
	}

	if overMemory(cost) {
		h.err = ErrOverMemory

		return 0, h.err
	}

	want := min((cost-FreeDecode+decodeChunk-1)/decodeChunk*decodeChunk, h.room)
	if want > h.held {
		// What it held when it had to wait is given back (bodyPool.regrow),
		// so a body that was planned too little, whose density was not in the
		// first FreeBody bytes, waits holding only its own bytes.
		err := h.pool.regrow(ctx, h.held, want)
		if err != nil {
			h.held, h.err = 0, err

			return 0, err
		}

		h.held = want
	}

	if h.held >= h.room {
		return math.MaxInt64, nil
	}

	return FreeDecode + h.held, nil
}

// Exempt makes the hold charge nothing, for an internal request
// (request.IsInternal): its caller's request was charged.
func (h *DecodeHold) Exempt() {
	h.free = true
}

// Release gives back what the hold charged. Decoding is over by then: a later
// Cover charges nothing. A nil hold holds nothing.
func (h *DecodeHold) Release() {
	if h == nil {
		return
	}

	h.free = true

	if h.held > 0 {
		h.pool.release(h.held)
		h.held = 0
	}
}

// planned is what Cover would hold from pool for a decoding that costs cost, for
// a hold that holds nothing yet: what passes FreeDecode, in whole steps, up to
// its room. It is zero for a hold that charges nothing or already holds some.
func (h *DecodeHold) planned(pool *bodyPool, cost int64) int64 {
	if h.free || h.pool != nil || cost <= FreeDecode {
		return 0
	}

	return min((cost-FreeDecode+decodeChunk-1)/decodeChunk*decodeChunk, pool.capacity-pool.bodyShare())
}

// prepaid makes the hold keep n bytes of pool that its body's meter took for it
// when the body was admitted (BodyMeter.admit), as if Cover had taken them.
func (h *DecodeHold) prepaid(pool *bodyPool, n int64) {
	if n > 0 {
		h.pool, h.room, h.held = pool, pool.capacity-pool.bodyShare(), n
	}
}
