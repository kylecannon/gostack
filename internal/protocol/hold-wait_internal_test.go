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
// length, more than the bound holds at once, is served in the order it
// arrives: each body is admitted in one step with the room its decoding is
// planned to hold, so no body holds part of the bound while it waits for more,
// and its decoding finds its room paid.
func Test_BodyMeter_UnknownLengthBurstServed(t *testing.T) {
	SetMaxBody(DefaultMaxBody)
	withBound(t, DefaultInFlight, DefaultBodyWait)

	const (
		// Each body is admitted alone, so the burst takes about 64 bodies
		// times 17 reads a millisecond apart, well within the wait.
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

	assert.Empty(t, pool.growing)
	assert.Empty(t, pool.starting)
}
