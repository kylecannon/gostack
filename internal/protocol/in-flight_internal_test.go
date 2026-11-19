package protocol

import (
	"bytes"
	"context"
	"io"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func withBound(t *testing.T, bound int64, wait time.Duration) {
	t.Helper()

	SetInFlight(bound)
	SetBodyWait(wait)
	t.Cleanup(func() {
		SetInFlight(0)
		SetBodyWait(DefaultBodyWait)
	})
}

func meteredBody(t *testing.T, size int) (*BodyMeter, context.Context) {
	t.Helper()

	m := NewBodyMeter(t.Context(), io.NopCloser(bytes.NewReader(make([]byte, size))))
	require.NotNil(t, m)

	return m, WithBodyMeter(t.Context(), m)
}

// Test_BodyMeter_ChargesOnlyWhenArmed: a body read without arming (a stream)
// holds nothing; one read by ReadBody holds about what it read until Release
// or ReleaseBody, and an empty pool after.
func Test_BodyMeter_ChargesOnlyWhenArmed(t *testing.T) {
	withBound(t, DefaultInFlight, time.Second)

	stream, _ := meteredBody(t, 4<<20)
	_, err := io.Copy(io.Discard, stream)
	require.NoError(t, err)

	_, held := InFlight()
	assert.Zero(t, held, "a stream is never charged")

	buffered, ctx := meteredBody(t, 4<<20)
	body, err := ReadBody(buffered, -1)
	require.NoError(t, err)
	require.Len(t, body, 4<<20)

	_, held = InFlight()
	assert.GreaterOrEqual(t, held, int64(4<<20))
	assert.LessOrEqual(t, held, int64(8<<20), "charged in steps that double, never past twice what was read")

	ReleaseBody(ctx)

	_, held = InFlight()
	assert.Zero(t, held, "ReleaseBody gives back what the body held")

	buffered.Release()

	_, held = InFlight()
	assert.Zero(t, held, "a second release gives back nothing")
}

// Test_BodyMeter_WaitsThenThrottles: a body that finds no room waits for one
// that holds it, and fails with ErrBusy when none comes within the wait.
func Test_BodyMeter_WaitsThenThrottles(t *testing.T) {
	SetMaxBody(DefaultMaxBody)
	withBound(t, DefaultMaxBody+1, 200*time.Millisecond)

	first, _ := meteredBody(t, int(DefaultMaxBody))
	_, err := ReadBody(first, -1)
	require.NoError(t, err)

	second, _ := meteredBody(t, 1<<20)
	_, err = ReadBody(second, -1)
	require.ErrorIs(t, err, ErrBusy, "no room within the wait")

	waiting, _ := meteredBody(t, 1<<20)
	done := make(chan error, 1)

	go func() {
		_, readErr := ReadBody(waiting, -1)
		done <- readErr
	}()

	time.Sleep(20 * time.Millisecond)
	first.Release()

	require.NoError(t, <-done, "room that comes within the wait admits the body")
	waiting.Release()

	_, held := InFlight()
	assert.Zero(t, held)
}

// Test_BodyPool_GrowingFirst: a body that already holds part of the pool is let
// in before one that holds none, so bodies part way through finish.
func Test_BodyPool_GrowingFirst(t *testing.T) {
	SetBodyWait(time.Second)
	t.Cleanup(func() { SetBodyWait(DefaultBodyWait) })

	pool := &bodyPool{capacity: 10}
	require.NoError(t, pool.acquire(t.Context(), 10, &pool.starting))

	order := make(chan string, 2)

	go func() {
		assert.NoError(t, pool.acquire(context.Background(), 5, &pool.starting))
		order <- "starting"
	}()

	for !queued(pool, &pool.starting) {
		runtime.Gosched()
	}

	go func() {
		assert.NoError(t, pool.acquire(context.Background(), 5, &pool.growing))
		order <- "growing"
	}()

	for !queued(pool, &pool.growing) {
		runtime.Gosched()
	}

	pool.release(5)
	assert.Equal(t, "growing", <-order)

	pool.release(5)
	assert.Equal(t, "starting", <-order)
}

func queued(pool *bodyPool, queue *[]*bodyWaiter) bool {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	return len(*queue) > 0
}

// Test_ReadBody_SizesByArrival: a body that states the cap and sends one byte
// allocates its first buffer, not the cap.
func Test_ReadBody_SizesByArrival(t *testing.T) {
	var before, after runtime.MemStats

	runtime.ReadMemStats(&before)

	body, err := ReadBody(bytes.NewReader([]byte("{")), DefaultMaxBody)

	runtime.ReadMemStats(&after)
	require.NoError(t, err)
	assert.Equal(t, "{", string(body))
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(2*FreeBody))
}
