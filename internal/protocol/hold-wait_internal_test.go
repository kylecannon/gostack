package protocol

import (
	"bytes"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_DecodeHold_NoHoldAndWait: bodies that each hold part of the bound, their
// bytes and the decode room they were admitted with, and that each need more
// room to decode than the bound has left, are all served: a hold that waits
// gives back the room it was admitted with and waits for all it needs, in
// order, so the first fits once the others gave theirs back, and each one
// served gives back room for the next. With a hold that kept its room while it
// waited, every hold would wait on the others until the wait ended.
func Test_DecodeHold_NoHoldAndWait(t *testing.T) {
	SetMaxBody(DefaultMaxBody)
	withBound(t, DefaultInFlight, 2*time.Second)

	const (
		size = 1 << 20
		// Each body is admitted with itself and 2 MiB of decode room: 21 of
		// them hold 63 of the 64 MiB.
		bodies = 21
		// Each decoding needs 8 MiB, more than the bound has left, and all of
		// them fit one after another beside the bodies' own 21 MiB.
		cost = FreeDecode + 8<<20
	)

	charges := make([]*Charge, bodies)

	for i := range charges {
		m, ctx := meteredBody(t, size, size)
		_, err := ReadBody(m, size)
		require.NoError(t, err)

		charges[i] = ChargeOf(ctx)
	}

	bound, held := InFlight()
	require.Greater(t, held, bound-(cost-FreeDecode), "no decoding fits beside what the bodies hold")

	var (
		wg   sync.WaitGroup
		errs = make([]error, bodies)
	)

	start := make(chan struct{})

	for i, c := range charges {
		wg.Go(func() {
			<-start

			_, errs[i] = c.HoldDecode(cost)

			c.Release()
		})
	}

	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "decoding %d is served", i)
	}

	_, held = InFlight()
	assert.Zero(t, held, "every request gives back all it held")
	assertQueuesEmpty(t)
}

// Test_BodyMeter_UnknownLengthBurstServed: a burst of bodies that state no
// length, more than the bound holds at once, is served: each step a body takes
// leaves every body being read a safe way to its claim, so none waits on bytes
// only a body behind it could give back, and its decoding finds its room paid.
func Test_BodyMeter_UnknownLengthBurstServed(t *testing.T) {
	SetMaxBody(DefaultMaxBody)
	withBound(t, DefaultInFlight, DefaultBodyWait)

	const (
		// At worst one body is read at a time, so the burst takes about 64
		// bodies times 17 reads a millisecond apart, well within the wait.
		size   = 1<<20 + 64
		bodies = 64
	)

	var (
		wg   sync.WaitGroup
		errs = make([]error, bodies)
	)

	start := make(chan struct{})

	for i := range bodies {
		c := new(Charge)
		ctx := WithCharge(t.Context(), c)
		m := NewBodyMeter(ctx, io.NopCloser(&slowReader{r: bytes.NewReader(make([]byte, size))}), -1)
		require.NotNil(t, m)
		c.Meter(m)
		ArmBody(ctx)

		wg.Go(func() {
			defer c.Release()

			<-start

			_, err := ReadBody(m, -1)
			if err != nil {
				errs[i] = err

				return
			}

			_, errs[i] = c.HoldDecode(TextDecode * size)
		})
	}

	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "body %d is served", i)
	}

	_, held := InFlight()
	assert.Zero(t, held, "every request gives back all it held")
	assertQueuesEmpty(t)
}

// pipedBody is a metered, armed body that states no length, read by ReadBody on
// its own goroutine from what the test writes to w. done gets ReadBody's error.
type pipedBody struct {
	charge *Charge
	w      *io.PipeWriter
	done   chan error
}

func newPipedBody(t *testing.T) *pipedBody {
	t.Helper()

	r, w := io.Pipe()
	c := new(Charge)
	ctx := WithCharge(t.Context(), c)
	m := NewBodyMeter(ctx, r, -1)
	require.NotNil(t, m)
	c.Meter(m)
	ArmBody(ctx)

	b := &pipedBody{charge: c, w: w, done: make(chan error, 1)}

	go func() {
		_, err := ReadBody(m, -1)
		b.done <- err
	}()

	t.Cleanup(func() { _ = w.Close() })

	return b
}

func (b *pipedBody) write(t *testing.T, n int) {
	t.Helper()

	_, err := b.w.Write(make([]byte, n))
	require.NoError(t, err)
}

// poolState reads what the pool's lock guards, for a test that waits on it.
func poolState[T any](read func(p *bodyPool) T) T {
	pool := inFlight.Load()
	pool.mu.Lock()
	defer pool.mu.Unlock()

	return read(pool)
}

// Test_BodyMeter_SlowUnknownLengthHoldsWhatItRead: bodies of unknown length
// still arriving hold what they have read, in steps, not the most they may
// hold, so a body that states its length and fits beside them is admitted at
// once. Were each admitted as the largest body it may be with its decode
// room, the second would wait for the first to end and the third and the
// stated body behind it, so the stated body would be throttled.
func Test_BodyMeter_SlowUnknownLengthHoldsWhatItRead(t *testing.T) {
	SetMaxBody(DefaultMaxBody)
	withBound(t, DefaultInFlight, 200*time.Millisecond)

	slow := make([]*pipedBody, 3)
	for i := range slow {
		slow[i] = newPipedBody(t)
		slow[i].write(t, 100<<10)
	}

	require.Eventually(t, func() bool {
		return poolState(func(p *bodyPool) int { return len(p.growers) }) == len(slow)
	}, 2*time.Second, time.Millisecond, "every slow body holds a step")

	_, held := InFlight()
	assert.LessOrEqual(t, held, int64(len(slow)*(256<<10)), "each holds a step near what it read")

	m, ctx := meteredBody(t, 1<<20, 1<<20)
	_, err := ReadBody(m, 1<<20)
	require.NoError(t, err, "a stated body that fits beside the slow ones is admitted at once")
	ReleaseBody(ctx)

	for _, b := range slow {
		b.write(t, 1)
		require.NoError(t, b.w.Close())
		require.NoError(t, <-b.done)
		b.charge.Release()
	}

	_, held = InFlight()
	assert.Zero(t, held, "every request gives back all it held")
	assertQueuesEmpty(t)
}

// slowReader reads at most 64 KiB at a time and yields between reads, as a
// body arriving in chunks does, so bodies read in step with one another.
type slowReader struct {
	r io.Reader
}

func (s *slowReader) Read(p []byte) (int, error) {
	time.Sleep(time.Millisecond)

	return s.r.Read(p[:min(len(p), FreeBody)])
}

func assertQueuesEmpty(t *testing.T) {
	t.Helper()

	pool := inFlight.Load()
	pool.mu.Lock()
	defer pool.mu.Unlock()

	assert.Empty(t, pool.steps)
	assert.Empty(t, pool.growing)
	assert.Empty(t, pool.starting)
	assert.Empty(t, pool.growers)
}

// Test_BodyMeter_StepKeepsWhatCoverTook: a body of unknown length decoded as
// it is read, whose decoding covers more than its steps took for it between
// two steps, gives back all it held once released. A step that set the hold to
// the room it planned, forgetting what Cover took past it, would leave those
// bytes charged to the pool for good.
func Test_BodyMeter_StepKeepsWhatCoverTook(t *testing.T) {
	SetMaxBody(DefaultMaxBody)
	withBound(t, DefaultInFlight, 200*time.Millisecond)

	c := new(Charge)
	ctx := WithCharge(t.Context(), c)
	m := NewBodyMeter(ctx, io.NopCloser(bytes.NewReader(make([]byte, 4<<20))), -1)
	require.NotNil(t, m)
	c.Meter(m)
	ArmBody(ctx)

	read := func(n int) {
		t.Helper()

		_, err := io.ReadFull(m, make([]byte, n))
		require.NoError(t, err)
	}

	read(1 << 20) // steps to 1 MiB, with 2 MiB of decode room
	require.Equal(t, int64(2<<20), c.decode.held, "the step's decode room")

	covered, err := c.HoldDecode(5 << 20)
	require.NoError(t, err)
	require.GreaterOrEqual(t, covered, int64(5<<20), "decoding covers more than the room")

	read(1<<20 + 1) // the next step, to 2 MiB with 4 MiB of room

	_, err = io.Copy(io.Discard, m)
	require.NoError(t, err)

	c.Release()

	_, held := InFlight()
	assert.Zero(t, held, "the request gives back all it held")
	assertQueuesEmpty(t)
}

// Test_BodyPool_SafetyBlockedStepStallsNoOne: while a body's step waits for a
// safe order, which only another body of unknown length reaching its end can
// give, bodies that state their length and fit are admitted at once, and a
// dense body is not refused for it. Were the step to hold up the queues behind
// it, each would wait until the younger body ended or its wait did. The two
// bodies of unknown length claim 48 MiB each of the 64: the older holds 16 MiB
// and the younger 40, so the older body's next step is safe only once the
// younger one ends.
func Test_BodyPool_SafetyBlockedStepStallsNoOne(t *testing.T) {
	SetMaxBody(DefaultMaxBody)
	withBound(t, DefaultInFlight, 2*time.Second)

	const mib = 1 << 20

	pool := inFlight.Load()
	older, younger := &grower{claim: 48 * mib}, &grower{claim: 48 * mib}
	ctx := t.Context()

	require.NoError(t, pool.step(ctx, older, mib))
	require.NoError(t, pool.step(ctx, younger, mib))
	require.NoError(t, pool.step(ctx, younger, 39*mib))
	require.NoError(t, pool.step(ctx, older, 15*mib))

	olderDone := make(chan error, 1)

	go func() { olderDone <- pool.step(ctx, older, mib) }()

	require.Eventually(t, func() bool {
		return poolState(func(p *bodyPool) int { return len(p.steps) }) == 1
	}, 2*time.Second, time.Millisecond, "the older body's step waits for a safe order")

	assert.False(t, BodiesWait(0), "a step that waits for a safe order holds up no dense body")

	for i := range 12 {
		size := 100<<10 + i*(900<<10)/11
		start := time.Now()
		m, callCtx := meteredBody(t, size, int64(size))
		_, err := ReadBody(m, int64(size))
		require.NoError(t, err, "call %d is admitted", i)
		assert.Less(t, time.Since(start), 100*time.Millisecond, "call %d is not held up", i)
		ReleaseBody(callCtx)
	}

	require.NoError(t, pool.step(ctx, younger, 8*mib), "the younger body reaches its claim")
	pool.finish(younger, 48*mib)
	require.NoError(t, <-olderDone, "the older body's step comes once the younger body ends")
	pool.finish(older, 17*mib)

	_, held := InFlight()
	assert.Zero(t, held, "every request gives back all it held")
	assertQueuesEmpty(t)
}
