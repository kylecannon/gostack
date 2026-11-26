package protocol

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests here are the user's rulings of 2026-11-25 on SQ-434's seam review,
// findings 1 and 3: a decoding that fits passes one that does not, and a
// body's first step that is not safe lets the new bodies behind it pass.

const passMiB = 1 << 20

// statedCharge is the Charge of a body that states its size, read whole.
func statedCharge(t *testing.T, size int) *Charge {
	t.Helper()

	m, ctx := meteredBody(t, size, int64(size))
	_, err := ReadBody(m, int64(size))
	require.NoError(t, err)

	return ChargeOf(ctx)
}

// readLater reads a body that states size in the background, as a new request,
// once the new bodies' queue holds it, and returns where its answer comes. The
// body is given back before the answer comes.
func readLater(t *testing.T, size int) <-chan error {
	t.Helper()

	m, ctx := meteredBody(t, size, int64(size))
	done := make(chan error, 1)

	go func() {
		_, err := ReadBody(m, int64(size))
		ChargeOf(ctx).Release()

		done <- err
	}()

	require.Eventually(t, func() bool {
		return poolState(func(p *bodyPool) int { return len(p.starting) }) == 1
	}, 2*time.Second, time.Millisecond, "the new body waits")

	return done
}

// decodeLater asks for c's decoding of cost in the background, once the
// decodings' queue holds queued, and returns where its answer comes. The
// request is given back before the answer comes.
func decodeLater(t *testing.T, c *Charge, cost int64, queued int) <-chan error {
	t.Helper()

	done := make(chan error, 1)

	go func() {
		_, err := c.HoldDecode(cost)
		c.Release()

		done <- err
	}()

	require.Eventually(t, func() bool {
		return poolState(func(p *bodyPool) int { return len(p.growing) }) == queued
	}, 2*time.Second, time.Millisecond, "the decoding waits")

	return done
}

// Test_DecodeHold_AFittingDecodingPassesAShortOne: two stated json bodies, A of
// 16 MiB admitted with 48 MiB and B of 4 MiB with 12. A's decoding needs
// 46 MiB, which does not fit beside the two bodies' 20, and waits. B's needs
// 16, which fits, and is granted at once past it; a stated 1 MiB body that
// arrives meanwhile waits behind A. When B's request ends, A fits and is
// served, and then the new body: no one is throttled. Were the decodings
// served strictly in order, B would wait behind A, and one of them would be
// throttled at the wait (SQ-434 seam review, finding 1).
func Test_DecodeHold_AFittingDecodingPassesAShortOne(t *testing.T) {
	SetMaxBody(DefaultMaxBody)
	withBound(t, DefaultInFlight, 2*time.Second)

	a, b := statedCharge(t, 16*passMiB), statedCharge(t, 4*passMiB)
	aDone := decodeLater(t, a, FreeDecode+46*passMiB, 1)
	newDone := readLater(t, passMiB)

	start := time.Now()
	_, err := b.HoldDecode(FreeDecode + 16*passMiB)
	require.NoError(t, err, "B's decoding is granted")
	assert.Less(t, time.Since(start), 100*time.Millisecond, "at once, past A's")

	assert.Equal(t, 1, poolState(func(p *bodyPool) int { return len(p.growing) }), "A's decoding still waits")
	assert.Equal(t, 1, poolState(func(p *bodyPool) int { return len(p.starting) }),
		"the new body waits behind a decoding that does not fit")

	b.Release()
	require.NoError(t, <-aDone, "A's decoding is served once B's request ends")
	require.NoError(t, <-newDone, "the new body is admitted after it")
	assert.Less(t, time.Since(start), time.Second, "before any wait ends")

	_, held := InFlight()
	assert.Zero(t, held, "every request gives back all it held")
	assertQueuesEmpty(t)
}

// Test_DecodeHold_APassedOverDecodingIsAnsweredAtItsWait: a decoding that does
// not fit, passed by one that does whose request keeps it, waits no longer
// than its own wait and is answered with ErrBusy then, holding no decode room:
// its wait is what bounds how long it is passed over. A new body that arrives
// behind it, with a longer wait, is not let in while it waits, and is admitted
// once it is answered. Every request gives back all it held.
func Test_DecodeHold_APassedOverDecodingIsAnsweredAtItsWait(t *testing.T) {
	const wait = 500 * time.Millisecond

	SetMaxBody(DefaultMaxBody)
	withBound(t, DefaultInFlight, wait)

	a, b := statedCharge(t, 16*passMiB), statedCharge(t, 4*passMiB)

	start := time.Now()
	aDone := decodeLater(t, a, FreeDecode+46*passMiB, 1)

	_, err := b.HoldDecode(FreeDecode + 16*passMiB)
	require.NoError(t, err, "B's decoding is granted past A's")

	SetBodyWait(4 * wait)

	newDone := readLater(t, passMiB)

	require.ErrorIs(t, <-aDone, ErrBusy, "A's decoding is answered at its wait")

	answered := time.Since(start)
	assert.GreaterOrEqual(t, answered, wait, "not before its wait")
	assert.Less(t, answered, 2*wait, "nor long after it")

	require.NoError(t, <-newDone, "the new body is admitted once A is answered")
	assert.Less(t, time.Since(start), 3*wait, "then, not at its own wait")

	b.Release()

	_, held := InFlight()
	assert.Zero(t, held, "every request gives back all it held")
	assertQueuesEmpty(t)
}

// Test_BodyPool_UnsafeFirstStepLetsNewBodiesPass: two bodies of unknown length
// claim 48 MiB each, the older holding 16 and the younger 24, so 24 MiB is
// free. A third body's 1 MiB first step would leave no safe order and waits
// for one of them to end; a stated 1 MiB body that arrives behind it is
// admitted at once. Were the first step to hold up the new bodies behind it,
// the stated body would wait until the first step's wait ended (SQ-434 seam
// review, finding 3). The first step comes once the younger body ends.
func Test_BodyPool_UnsafeFirstStepLetsNewBodiesPass(t *testing.T) {
	SetMaxBody(DefaultMaxBody)
	withBound(t, DefaultInFlight, 2*time.Second)

	pool := inFlight.Load()
	older, younger, third := &grower{claim: 48 * passMiB}, &grower{claim: 48 * passMiB}, &grower{claim: 48 * passMiB}
	ctx := t.Context()

	require.NoError(t, pool.step(ctx, older, passMiB, true))
	require.NoError(t, pool.step(ctx, younger, passMiB, true))
	require.NoError(t, pool.step(ctx, younger, 23*passMiB, true))
	require.NoError(t, pool.step(ctx, older, 15*passMiB, true))

	thirdDone := make(chan error, 1)

	go func() { thirdDone <- pool.step(ctx, third, passMiB, true) }()

	require.Eventually(t, func() bool {
		return poolState(func(p *bodyPool) int { return len(p.starting) }) == 1
	}, 2*time.Second, time.Millisecond, "the third body's first step waits")

	assert.False(t, BodiesWait(0), "a first step that waits for a safe order holds up no one")

	start := time.Now()
	m, callCtx := meteredBody(t, passMiB, passMiB)
	_, err := ReadBody(m, passMiB)
	require.NoError(t, err, "the stated body is admitted")
	assert.Less(t, time.Since(start), 100*time.Millisecond, "at once, not at the first step's wait")
	ReleaseBody(callCtx)

	assert.Equal(t, 1, poolState(func(p *bodyPool) int { return len(p.starting) }), "the first step still waits")

	pool.finish(younger, 24*passMiB)
	require.NoError(t, <-thirdDone, "the first step comes once the younger body ends")
	pool.finish(older, 16*passMiB)
	pool.finish(third, passMiB)

	_, held := InFlight()
	assert.Zero(t, held, "every request gives back all it held")
	assertQueuesEmpty(t)
}

// Test_BodyPool_APassedOverFirstStepIsAnsweredAtItsWait: a body's first step
// that would leave no safe order, passed by stated bodies that keep arriving,
// waits no longer than its own wait and is answered with ErrBusy then, holding
// nothing: its wait is what bounds how long it is passed over. Every request
// gives back all it held, and no body of unknown length is left recorded.
func Test_BodyPool_APassedOverFirstStepIsAnsweredAtItsWait(t *testing.T) {
	const wait = 500 * time.Millisecond

	SetMaxBody(DefaultMaxBody)
	withBound(t, DefaultInFlight, wait)

	pool := inFlight.Load()
	older, younger, third := &grower{claim: 48 * passMiB}, &grower{claim: 48 * passMiB}, &grower{claim: 48 * passMiB}
	ctx := t.Context()

	require.NoError(t, pool.step(ctx, older, passMiB, true))
	require.NoError(t, pool.step(ctx, younger, passMiB, true))
	require.NoError(t, pool.step(ctx, younger, 23*passMiB, true))
	require.NoError(t, pool.step(ctx, older, 15*passMiB, true))

	start := time.Now()
	thirdDone := make(chan error, 1)

	go func() { thirdDone <- pool.step(ctx, third, passMiB, true) }()

	require.Eventually(t, func() bool {
		return poolState(func(p *bodyPool) int { return len(p.starting) }) == 1
	}, 2*time.Second, time.Millisecond, "the third body's first step waits")

	var (
		thirdErr error
		passed   int
	)

	for thirdErr == nil {
		select {
		case thirdErr = <-thirdDone:
			require.ErrorIs(t, thirdErr, ErrBusy, "the first step is answered")
		default:
			called := time.Now()
			m, callCtx := meteredBody(t, passMiB, passMiB)
			_, err := ReadBody(m, passMiB)
			require.NoError(t, err, "stated body %d is admitted", passed)
			// Well inside the wait, which a body held up by the first step
			// would wait out; at -race -count=20 beside the gateway's tests
			// one read of 1 MiB took 139 ms.
			assert.Less(t, time.Since(called), wait/2, "stated body %d passes without waiting on it", passed)
			ReleaseBody(callCtx)

			passed++

			time.Sleep(10 * time.Millisecond)
		}
	}

	answered := time.Since(start)
	assert.GreaterOrEqual(t, answered, wait, "not before its wait")
	assert.Less(t, answered, 2*wait, "nor long after it")
	assert.Greater(t, passed, 1, "stated bodies kept passing it")

	pool.finish(younger, 24*passMiB)
	pool.finish(older, 16*passMiB)

	_, held := InFlight()
	assert.Zero(t, held, "every request gives back all it held")
	assertQueuesEmpty(t)
}
