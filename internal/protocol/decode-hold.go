package protocol

import (
	"context"
	"math"
)

const (
	// FreeDecode is what decoding one request may hold before what it holds is
	// charged against the in-flight bound (SetInFlight). Every ordinary request
	// decodes into less, so it takes no lock and no atomic for it.
	FreeDecode = 1 << 20

	// decodeChunk is the step a decoding's charge grows by past FreeDecode.
	decodeChunk = 1 << 20
)

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
// nothing yet. Its zero value holds nothing. It is used on the request's own
// goroutine.
type DecodeHold struct {
	pool *bodyPool
	held int64
	room int64 // the most it charges: the bound less one body at its cap
	free bool  // charges nothing: no bound, an internal request, or released
	err  error
}

// Cover makes the hold cover cost bytes of decoding for a request whose
// context is ctx, waiting for room as a body does, and returns what it covers.
// One request's charge stops at the bound less one body at
// MaxBody, so a request alone always fits whatever the bound and the
// service's decode factor; what it decodes beyond that is not charged.
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

		body := pool.capacity
		if limit := MaxBody(); limit > 0 {
			body = min(body, limit+1)
		}

		h.pool, h.room = pool, pool.capacity-body
	}

	want := min((cost-FreeDecode+decodeChunk-1)/decodeChunk*decodeChunk, h.room)
	if want > h.held {
		err := h.pool.acquire(ctx, want-h.held, &h.pool.growing)
		if err != nil {
			h.err = err

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
// Cover charges nothing.
func (h *DecodeHold) Release() {
	h.free = true

	if h.held > 0 {
		h.pool.release(h.held)
		h.held = 0
	}
}
