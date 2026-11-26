package protocol

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_BodyPool_StopEndsTheWaits: bodies queued for room, a stated one and a
// step of one of unknown length, stop waiting when their request's stop
// (StopWaitsWith) closes, long before the body wait, fail with ErrBusy and
// leave the pool as it was.
func Test_BodyPool_StopEndsTheWaits(t *testing.T) {
	SetBodyWait(time.Minute)
	t.Cleanup(func() { SetBodyWait(DefaultBodyWait) })

	p := &bodyPool{capacity: 64}
	ctx := t.Context()
	g := &grower{claim: 32}

	require.NoError(t, p.acquire(ctx, 56, &p.starting))
	require.NoError(t, p.step(ctx, g, 8))

	stop := make(chan struct{})
	stopped := StopWaitsWith(ctx, stop)
	done := make(chan error, 2)

	go func() { done <- p.acquire(stopped, 8, &p.starting) }()
	go func() { done <- p.step(stopped, g, 8) }()

	require.Eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()

		return len(p.starting) == 1 && len(p.steps) == 1
	}, time.Second, time.Millisecond, "both wait")

	start := time.Now()

	close(stop)

	for range 2 {
		require.ErrorIs(t, <-done, ErrBusy)
	}

	assert.Less(t, time.Since(start), time.Second, "answered as the stop closes, not at the wait")

	p.finish(g, 8)
	p.release(56)

	p.mu.Lock()
	defer p.mu.Unlock()

	assert.Zero(t, p.used)
	assert.Empty(t, p.starting)
	assert.Empty(t, p.steps)
}
