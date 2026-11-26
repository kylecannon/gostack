package protocol

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_BodyPool_StopEndsOnlyTheQueuedWaits: when the request's stop (Charge.StopWaitsWith)
// closes, a body queued for admission whose request holds nothing stops
// waiting at once, long before the body wait, and fails with ErrBusy. A body
// of unknown length that holds some of the pool and waits for its next step,
// and a first step whose request holds decoding room, keep waiting, and are
// granted once room is given back. The pool ends as it began.
func Test_BodyPool_StopEndsOnlyTheQueuedWaits(t *testing.T) {
	SetBodyWait(time.Minute)
	t.Cleanup(func() { SetBodyWait(DefaultBodyWait) })

	stop := make(chan struct{})
	c := new(Charge)
	ctx := WithCharge(t.Context(), c)
	c.StopWaitsWith(stop)

	p := &bodyPool{capacity: 64}
	running := &grower{claim: 32}
	holding := &grower{claim: 16}

	require.NoError(t, p.acquire(ctx, 56, &p.starting))
	require.NoError(t, p.step(ctx, running, 8, true))

	queued := make(chan error, 1)
	kept := make(chan error, 2)

	go func() {
		queued <- p.take(ctx, &bodyWaiter{n: 8, ready: make(chan struct{}), bare: true}, &p.starting)
	}()
	go func() { kept <- p.step(ctx, running, 8, true) }()
	go func() { kept <- p.step(ctx, holding, 8, false) }()

	require.Eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()

		return len(p.starting) == 2 && len(p.steps) == 1
	}, time.Second, time.Millisecond, "all three wait")

	start := time.Now()

	close(stop)

	require.ErrorIs(t, <-queued, ErrBusy)
	assert.Less(t, time.Since(start), time.Second, "answered as the stop closes, not at the wait")

	select {
	case err := <-kept:
		t.Fatalf("a body that holds some of the pool stopped waiting: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	p.release(56)

	for range 2 {
		require.NoError(t, <-kept, "granted once room is given back")
	}

	p.finish(running, 16)
	p.finish(holding, 8)

	p.mu.Lock()
	defer p.mu.Unlock()

	assert.Zero(t, p.used)
	assert.Empty(t, p.starting)
	assert.Empty(t, p.steps)
	assert.Empty(t, p.growers)
}

// Test_DecodeHold_StopEndsADecodingThatHoldsNothing: a decoding whose request
// holds nothing of the bound, neither body bytes nor decode room (a body short
// enough not to be metered), is answered as the shutdown stop closes, as a
// body queued for admission is; a decoding whose request's body holds bytes
// keeps waiting and is granted once room is given back. Both requests carry
// the stop (Charge.StopWaitsWith).
func Test_DecodeHold_StopEndsADecodingThatHoldsNothing(t *testing.T) {
	const mib = 1 << 20

	stop := make(chan struct{})

	SetMaxBody(DefaultMaxBody)
	withBound(t, DefaultInFlight, time.Minute)

	m, holdingCtx := meteredBody(t, 2*mib, 2*mib)
	_, err := ReadBody(m, 2*mib)
	require.NoError(t, err)

	holding := ChargeOf(holdingCtx)
	holding.StopWaitsWith(stop)

	pool := inFlight.Load()
	_, held := InFlight()
	require.NoError(t, pool.acquire(t.Context(), DefaultInFlight-held, &pool.starting), "the rest of the bound")

	kept := make(chan error, 1)

	go func() {
		_, err := holding.HoldDecode(FreeDecode + 8*mib)
		kept <- err
	}()

	require.Eventually(t, func() bool {
		return poolState(func(p *bodyPool) int { return len(p.growing) }) == 1
	}, time.Second, time.Millisecond, "the decoding of the body that holds bytes waits")

	bare := new(Charge)
	bareCtx := WithCharge(t.Context(), bare)
	bare.StopWaitsWith(stop)

	stopped := make(chan error, 1)

	go func() {
		// More than the decoding above gave back: one that fits would be let
		// in past it (grant).
		_, err := bare.HoldDecode(FreeDecode + 8*mib)
		stopped <- err
	}()

	require.Eventually(t, func() bool {
		return poolState(func(p *bodyPool) int { return len(p.growing) }) == 2
	}, time.Second, time.Millisecond, "the decoding that holds nothing waits")

	start := time.Now()

	close(stop)

	require.ErrorIs(t, <-stopped, ErrBusy)
	assert.Less(t, time.Since(start), time.Second, "answered as the stop closes, not at the wait")

	select {
	case err := <-kept:
		t.Fatalf("a decoding whose body holds bytes stopped waiting: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	pool.release(DefaultInFlight - held)
	require.NoError(t, <-kept, "granted once room is given back")

	holding.Release()
	ChargeOf(bareCtx).Release()

	_, held = InFlight()
	assert.Zero(t, held, "every request gives back all it held")
	assertQueuesEmpty(t)
}
