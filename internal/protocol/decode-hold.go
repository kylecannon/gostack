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
	err  error
	// body is the request's body meter (Charge.Meter), or nil: a hold that
	// holds nothing waits as a request holding nothing of the pool only while
	// its body holds nothing either (bodyWaiter.bare).
	body *BodyMeter
	free bool // charges nothing: no bound, an internal request, or released
	// spent is whether a Cover has returned what it holds as covered, so the
	// decoder may hold those bytes already: a decoder that charges as it
	// decodes (restxml). Until then what it holds is room its body was
	// admitted with (prepaid), which nothing has decoded into yet.
	spent bool
}

// Cover makes the hold cover cost bytes of decoding for a request whose
// context is ctx, waiting for room as a body does, and returns what it covers.
// A hold that has to wait before any Cover returned gives back what it holds,
// which nothing has decoded into, and waits for all it needs, let in once it
// fits whether or not the holds that waited before it do (bodyPool); on
// ErrBusy it holds nothing. One that a Cover returned before keeps what it
// holds while it waits, since the decoder holds those bytes, and on ErrBusy
// keeps them until Release.
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
		h.spent = true

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

	want := decodeRoom(h.pool, cost)
	if want > h.held {
		err := h.grow(ctx, want)
		if err != nil {
			h.err = err

			return 0, err
		}
	}

	h.spent = true

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
		h.pool.giveBack(h.held, h.owner())
		h.held = 0
	}
}

// Held is what the hold holds of the in-flight bound now. A nil hold holds
// nothing.
func (h *DecodeHold) Held() int64 {
	if h == nil {
		return 0
	}

	return h.held
}

// grow makes the hold take want, as Cover describes: a hold that nothing has
// decoded into gives back what it holds while it waits (regrow), so holds that
// each hold part of the bound never wait on one another for it. Its request's
// body bytes stay held while it waits (bodyPool).
func (h *DecodeHold) grow(ctx context.Context, want int64) error {
	bare := h.held == 0 && (h.body == nil || h.body.held == 0)
	owner := h.owner()

	if !h.spent {
		err := h.pool.regrow(ctx, h.held, want, bare, owner)
		if err != nil {
			h.held = 0

			return err
		}

		h.held = want

		return nil
	}

	w := &bodyWaiter{n: want - h.held, owner: owner, ready: make(chan struct{}), bare: bare}

	err := h.pool.take(ctx, w, &h.pool.growing)
	if err != nil {
		return err
	}

	h.held = want

	return nil
}

// owner is the body of unknown length the request reads while it is read
// (BodyMeter.grower), which what the hold holds is counted to in the pool's
// safe order (bodyWaiter.owner), or nil.
func (h *DecodeHold) owner() *grower {
	if h.body == nil {
		return nil
	}

	return h.body.grower
}

// planned is what Cover would hold from pool for a decoding that costs cost, for
// a hold that holds nothing yet: what passes FreeDecode, in whole steps, up to
// its room. It is zero for a hold that charges nothing or already holds some.
func (h *DecodeHold) planned(pool *bodyPool, cost int64) int64 {
	if h.free || h.pool != nil {
		return 0
	}

	return decodeRoom(pool, cost)
}

// decodeRoom is what Cover holds from pool for a decoding that costs cost:
// what passes FreeDecode, in whole steps, up to the bound less one body.
func decodeRoom(pool *bodyPool, cost int64) int64 {
	if cost <= FreeDecode {
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
