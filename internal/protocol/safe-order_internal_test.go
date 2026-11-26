package protocol

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests here hold the bodies of unknown length on a pool of 64 bytes whose
// claims are 48 each, so which order lets each reach its claim is plain.

// newSafeOrderPool is a pool of 64 bytes and, for the body wait of wait, the
// older and the younger of two bodies of unknown length claiming 48 each: the
// older holds 16 and the younger 40, so only the younger can reach its claim
// first, with the 8 left.
func newSafeOrderPool(t *testing.T, wait time.Duration) (p *bodyPool, older, younger *grower) {
	t.Helper()

	SetBodyWait(wait)
	t.Cleanup(func() { SetBodyWait(DefaultBodyWait) })

	p = &bodyPool{capacity: 64}
	older, younger = &grower{claim: 48}, &grower{claim: 48}
	ctx := t.Context()

	require.NoError(t, p.step(ctx, older, 1, true))
	require.NoError(t, p.step(ctx, younger, 1, true))
	require.NoError(t, p.step(ctx, younger, 39, true), "the younger body may grow: it can finish first")
	require.NoError(t, p.step(ctx, older, 15, true), "8 left, the younger body's need")

	return p, older, younger
}

// stepLater asks for g's step of n in the background, once the steps queue
// holds queued steps, and returns where its answer comes.
func stepLater(t *testing.T, p *bodyPool, g *grower, n int64, queued int) <-chan error {
	t.Helper()

	done := make(chan error, 1)

	go func() { done <- p.step(t.Context(), g, n, true) }()

	require.Eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()

		return len(p.steps) == queued
	}, time.Second, time.Millisecond, "the step waits")

	return done
}

// Test_BodyPool_YoungerFinishesThenOlder: an older body's step that is safe
// only once a younger body ends waits, and the younger body's step, which
// takes it to its claim, passes it. The younger body ends, and the older
// body's step comes then. Were the younger body's step queued behind the
// older one's, each would wait on the other until the wait ended.
func Test_BodyPool_YoungerFinishesThenOlder(t *testing.T) {
	p, older, younger := newSafeOrderPool(t, 300*time.Millisecond)
	olderDone := stepLater(t, p, older, 1, 1)

	start := time.Now()
	require.NoError(t, p.step(t.Context(), younger, 8, true), "the younger body reaches its claim")
	assert.Less(t, time.Since(start), 100*time.Millisecond, "without waiting out the older body's step")

	select {
	case err := <-olderDone:
		t.Fatalf("the older body's step came before the younger body ended: %v", err)
	default:
	}

	p.finish(younger, 48)
	require.NoError(t, <-olderDone, "the older body's step comes once the younger body ends")
	assert.Less(t, time.Since(start), 100*time.Millisecond)

	p.finish(older, 17)
	assert.Zero(t, p.used, "the pool ends empty")
}

// Test_BodyPool_SafetyBlockedStepWaitsForOneYoungerBody: an older body's step
// that waits for a safe order is granted when the younger body it waits on
// ends, ahead of a body admitted behind it, and does not wait for a body that
// came after it.
func Test_BodyPool_SafetyBlockedStepWaitsForOneYoungerBody(t *testing.T) {
	p, older, younger := newSafeOrderPool(t, 2*time.Second)
	olderDone := stepLater(t, p, older, 1, 1)

	// A third body's first step waits too: it would leave no safe order.
	third := &grower{claim: 48}
	thirdDone := make(chan error, 1)

	go func() { thirdDone <- p.step(t.Context(), third, 1, true) }()

	require.Eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()

		return len(p.starting) == 1
	}, time.Second, time.Millisecond, "the third body waits to start")

	require.NoError(t, p.step(t.Context(), younger, 8, true))
	p.finish(younger, 48)

	select {
	case err := <-olderDone:
		require.NoError(t, err, "the older body's step comes when the younger body ends")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("the older body's step waited past the younger body's end")
	}

	require.NoError(t, <-thirdDone, "the third body starts after it")

	p.finish(older, 17)
	p.finish(third, 1)
	assert.Zero(t, p.used, "the pool ends empty")
}

// Test_BodyMeter_FastBodyBehindSlowOnesServed: a fast body of unknown length
// that arrives after three slow ones, each holding a step near what it has
// read, is read to its end at once. The slow bodies can still reach their
// claims once it finishes and gives its bytes back, so its steps leave the
// pool safe. Were its steps safe only when the slow bodies could reach their
// claims first, in the order they came, it would wait for them to end.
func Test_BodyMeter_FastBodyBehindSlowOnesServed(t *testing.T) {
	SetMaxBody(DefaultMaxBody)
	withBound(t, DefaultInFlight, 2*time.Second)

	slow := make([]*pipedBody, 3)
	for i := range slow {
		slow[i] = newPipedBody(t)
		slow[i].write(t, 100<<10)
	}

	require.Eventually(t, func() bool {
		return poolState(func(p *bodyPool) int { return len(p.growers) }) == len(slow)
	}, 2*time.Second, time.Millisecond, "every slow body holds a step")

	start := time.Now()
	fast := newPipedBody(t)

	for range 16 {
		fast.write(t, 1<<20)
	}

	require.NoError(t, fast.w.Close())
	require.NoError(t, <-fast.done, "the fast body is read to its end")
	assert.Less(t, time.Since(start), 500*time.Millisecond, "it does not wait for the slow bodies")
	fast.charge.Release()

	for _, b := range slow {
		b.write(t, 1)
		require.NoError(t, b.w.Close())
		require.NoError(t, <-b.done)
		b.charge.Release()
	}

	_, held := InFlight()
	assert.Zero(t, held, "every request gives back all it held")
	assertQueuesEmpty(t)
}
