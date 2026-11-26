package protocol

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests here are SQ-434's second seam review: the decode room a body of
// unknown length's request holds through Cover counts as that body's in the
// safe order (A1), a decoding that fits passes a step short of room (A2), and
// a grant that lands as the shutdown stop closes is kept (B4). They run on a
// pool of 64 MiB whose bodies of unknown length claim 48 MiB each.

// coverPool is a pool of 64 MiB, with the body wait of wait, for decode holds
// built on it.
func coverPool(t *testing.T, wait time.Duration) *bodyPool {
	t.Helper()

	SetMaxBody(DefaultMaxBody)
	SetBodyWait(wait)
	t.Cleanup(func() { SetBodyWait(DefaultBodyWait) })

	return &bodyPool{capacity: 64 * passMiB}
}

// coverHold is a decode hold on p that holds held, room its body was admitted
// with; spent is whether a Cover has returned it. g is the body of unknown
// length the request reads, or nil.
func coverHold(p *bodyPool, held int64, spent bool, g *grower) *DecodeHold {
	return &DecodeHold{
		pool: p, room: p.capacity - p.bodyShare(), held: held, spent: spent,
		body: &BodyMeter{grower: g},
	}
}

// acquireLater queues a body of n in p.starting in the background, once it
// waits, and returns where its answer comes.
func acquireLater(t *testing.T, p *bodyPool, n int64) <-chan error {
	t.Helper()

	done := make(chan error, 1)

	go func() { done <- p.acquire(t.Context(), n, &p.starting) }()

	require.Eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()

		return len(p.starting) == 1
	}, time.Second, time.Millisecond, "the new body waits")

	return done
}

// Test_DecodeHold_CoverRoomCountsToItsGrowingBody: G2, a body of unknown
// length holding 4 MiB and the 8 MiB of decode room its step took, raises its
// decoding to 23 MiB through Cover while it is read, so its request holds
// 27 MiB. G1 holds 24 MiB and asks for 24 more. Counting G2 at 27, no order
// lets both reach their claims, so G1's step waits for G2 to end, not for
// room: a stated 1 MiB body is admitted past it at once, and so is G2's next
// 1 MiB of decoding. Counting G2 at its steps' 12, G1's step would look safe
// and short of room, and stop the stated body and G2's decoding until their
// waits ended.
func Test_DecodeHold_CoverRoomCountsToItsGrowingBody(t *testing.T) {
	const wait = 2 * time.Second

	p := coverPool(t, wait)
	ctx := t.Context()
	g1, g2 := &grower{claim: 48 * passMiB}, &grower{claim: 48 * passMiB}

	require.NoError(t, p.step(ctx, g1, 24*passMiB, true))
	require.NoError(t, p.step(ctx, g2, 12*passMiB, true))

	h := coverHold(p, 8*passMiB, true, g2)
	_, err := h.Cover(ctx, FreeDecode+23*passMiB)
	require.NoError(t, err, "G2's decoding grows to 23 MiB: it fits")

	g1Done := stepLater(t, p, g1, 24*passMiB)

	p.mu.Lock()
	blocked := p.blocked(p.steps[0])
	p.mu.Unlock()
	assert.True(t, blocked, "G1's step waits for a safe order, counting the 27 MiB G2's request holds")

	start := time.Now()
	require.NoError(t, p.acquire(ctx, passMiB, &p.starting), "a stated body is admitted past G1's step")
	assert.Less(t, time.Since(start), wait/2, "at once")

	start = time.Now()
	_, err = h.Cover(ctx, FreeDecode+24*passMiB)
	require.NoError(t, err, "G2's next 1 MiB of decoding fits and is granted")
	assert.Less(t, time.Since(start), wait/2, "at once")

	h.body.grower = nil // as BodyMeter.Release does

	p.finish(g2, 4*passMiB)
	h.Release()
	require.NoError(t, <-g1Done, "G1's step comes once G2 ends")

	p.finish(g1, 48*passMiB)
	p.release(passMiB)
	assert.Zero(t, p.used, "the pool ends empty")
}

// Test_DecodeHold_AFittingDecodingPassesAShortStep: R, a stated 10 MiB body
// admitted with 20 MiB of decode room that a Cover has returned, and G, a
// body of unknown length holding 24 MiB whose next step of 24 is safe and
// short of room. R's decoding asks for 1 MiB more, which fits, and is granted
// at once past G's step; a stated 1 MiB body that arrives meanwhile waits
// behind the step. When R's request ends, G's step comes, then the new body.
func Test_DecodeHold_AFittingDecodingPassesAShortStep(t *testing.T) {
	const wait = 2 * time.Second

	p := coverPool(t, wait)
	ctx := t.Context()
	g := &grower{claim: 48 * passMiB}

	require.NoError(t, p.acquire(ctx, 30*passMiB, &p.starting))
	require.NoError(t, p.step(ctx, g, 24*passMiB, true))

	r := coverHold(p, 20*passMiB, true, nil)
	gDone := stepLater(t, p, g, 24*passMiB)

	start := time.Now()
	_, err := r.Cover(ctx, FreeDecode+21*passMiB)
	require.NoError(t, err, "R's decoding is granted past G's short step")
	assert.Less(t, time.Since(start), wait/2, "at once")

	newDone := acquireLater(t, p, passMiB)

	p.release(10 * passMiB)
	r.Release()
	require.NoError(t, <-gDone, "G's step comes once R's request ends")
	require.NoError(t, <-newDone, "the new body after it")

	p.finish(g, 48*passMiB)
	p.release(passMiB)
	assert.Zero(t, p.used, "the pool ends empty")
}

// Test_DecodeHold_AFittingRegrowKeepsItsRoom: as
// Test_DecodeHold_AFittingDecodingPassesAShortStep, but R's room has not been
// returned by a Cover, so R's decoding gives it back to wait for all it needs
// (regrow). It fits where it is, beside G's short step, and is granted there
// at once. Were the room given back first, G's step would take it and R's
// decoding would wait for G's body to end.
func Test_DecodeHold_AFittingRegrowKeepsItsRoom(t *testing.T) {
	const wait = 2 * time.Second

	p := coverPool(t, wait)
	ctx := t.Context()
	g := &grower{claim: 48 * passMiB}

	require.NoError(t, p.acquire(ctx, 30*passMiB, &p.starting))
	require.NoError(t, p.step(ctx, g, 24*passMiB, true))

	r := coverHold(p, 20*passMiB, false, nil)
	gDone := stepLater(t, p, g, 24*passMiB)

	start := time.Now()
	_, err := r.Cover(ctx, FreeDecode+21*passMiB)
	require.NoError(t, err, "R's decoding is granted where it is")
	assert.Less(t, time.Since(start), wait/2, "at once")

	p.mu.Lock()
	stepWaits := len(p.steps) == 1
	p.mu.Unlock()
	assert.True(t, stepWaits, "G's step still waits for room")

	p.release(10 * passMiB)
	r.Release()
	require.NoError(t, <-gDone, "G's step comes once R's request ends")

	p.finish(g, 48*passMiB)
	assert.Zero(t, p.used, "the pool ends empty")
}

// Test_BodyPool_AGrantFoundAtTheStopIsKept: a waiter granted as the shutdown
// stop closes keeps what it was granted and goes on, whichever its wait sees
// first: the stop only ends a wait that has not been granted.
func Test_BodyPool_AGrantFoundAtTheStopIsKept(t *testing.T) {
	stop := make(chan struct{})
	close(stop)

	for range 300 {
		p := &bodyPool{capacity: 64, used: 8}
		ready := make(chan struct{})
		close(ready)

		w := &bodyWaiter{n: 8, ready: ready, stop: stop}
		require.NoError(t, p.wait(t.Context(), w, &p.starting), "the grant is kept")
		require.Equal(t, int64(8), p.used, "and not given back")
	}
}
