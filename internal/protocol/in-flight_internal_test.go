package protocol

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"testing"
	"testing/iotest"
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

// streamBody is a metered body of size bytes that states length (-1: none), on
// a request Charge, not armed: a stream.
func streamBody(t *testing.T, size int, length int64) (*BodyMeter, context.Context) {
	t.Helper()

	c := new(Charge)
	ctx := WithCharge(t.Context(), c)
	m := NewBodyMeter(ctx, io.NopCloser(bytes.NewReader(make([]byte, size))), length)
	require.NotNil(t, m)
	c.Meter(m)

	return m, ctx
}

// meteredBody is streamBody armed through its context, as ReadRequest and
// rest.CapBody arm it.
func meteredBody(t *testing.T, size int, length int64) (*BodyMeter, context.Context) {
	t.Helper()

	m, ctx := streamBody(t, size, length)
	ArmBody(ctx)

	return m, ctx
}

// Test_BodyMeter_ChargesOnlyWhenArmed: a body read without arming (a stream)
// holds nothing; one read by ReadBody holds about what it read until Release
// or ReleaseBody, and an empty pool after.
func Test_BodyMeter_ChargesOnlyWhenArmed(t *testing.T) {
	withBound(t, DefaultInFlight, time.Second)

	stream, _ := streamBody(t, 4<<20, -1)
	_, err := io.Copy(io.Discard, stream)
	require.NoError(t, err)

	_, held := InFlight()
	assert.Zero(t, held, "a stream is never charged")

	buffered, ctx := meteredBody(t, 4<<20, -1)
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
	withBound(t, 1, 200*time.Millisecond) // raised to two bodies at the cap

	bound, _ := InFlight()
	require.Equal(t, 2*(DefaultMaxBody+1), bound, "the bound has room for a body and its decoding")

	first, _ := meteredBody(t, int(DefaultMaxBody), -1)
	_, err := ReadBody(first, -1)
	require.NoError(t, err)

	filler, _ := meteredBody(t, int(DefaultMaxBody), -1)
	_, err = ReadBody(filler, -1)
	require.NoError(t, err)

	defer filler.Release()

	second, _ := meteredBody(t, 1<<20, -1)
	_, err = ReadBody(second, -1)
	require.ErrorIs(t, err, ErrBusy, "no room within the wait")

	waiting, _ := meteredBody(t, 1<<20, -1)
	done := make(chan error, 1)

	go func() {
		_, readErr := ReadBody(waiting, -1)
		done <- readErr
	}()

	time.Sleep(20 * time.Millisecond)
	first.Release()

	require.NoError(t, <-done, "room that comes within the wait admits the body")
	waiting.Release()
	filler.Release()

	_, held := InFlight()
	assert.Zero(t, held)
}

// Test_BodyMeter_StatedLengthAtOnce: a body that states its length is charged
// all of it the first time it reads past FreeBody, with the room its decoding
// is planned to hold, and holds nothing before: one that waits for room holds
// nothing while it waits, and decoding an ordinary body takes nothing more, so
// bodies admitted never wait on each other to grow or to decode.
func Test_BodyMeter_StatedLengthAtOnce(t *testing.T) {
	SetMaxBody(DefaultMaxBody)
	withBound(t, 1, 100*time.Millisecond)

	const (
		size = 4 << 20
		// room is TextDecode times the body, and FreeBody for its structure,
		// past FreeDecode, in whole steps.
		room = 8 << 20
	)

	m, ctx := meteredBody(t, size, size)
	buf := make([]byte, FreeBody)

	_, err := io.ReadFull(m, buf)
	require.NoError(t, err)

	_, held := InFlight()
	assert.Zero(t, held, "nothing is charged up to FreeBody")

	_, err = io.ReadFull(m, buf[:1])
	require.NoError(t, err)

	_, held = InFlight()
	assert.Equal(t, int64(size+room), held, "the stated length and the decode room are charged at once")

	_, err = io.Copy(io.Discard, m)
	require.NoError(t, err)

	covered, err := ChargeOf(ctx).HoldDecode(TextDecode * size)
	require.NoError(t, err)
	assert.Equal(t, int64(FreeDecode+room), covered, "decoding what was planned is covered")

	_, held = InFlight()
	assert.Equal(t, int64(size+room), held, "and never grows")

	ChargeOf(ctx).ReleaseDecode()

	_, held = InFlight()
	assert.Equal(t, int64(size), held, "the decode room is given back with the decoding")

	// The rest of the bound is taken, so the next body waits holding nothing
	// and gives up holding nothing.
	rest, _ := meteredBody(t, int(DefaultMaxBody), -1)
	_, err = ReadBody(rest, -1)
	require.NoError(t, err)

	defer rest.Release()

	bound, held := InFlight()
	late, _ := meteredBody(t, int(bound-held)+1, bound-held+1)
	_, err = ReadBody(late, -1)
	require.ErrorIs(t, err, ErrBusy)

	_, after := InFlight()
	assert.Equal(t, held, after, "a body refused all at once held nothing")

	m.Release()
}

// Test_BodyMeter_DecodeRoomByReader: the room a body is admitted with follows
// the reader that arms it, and a lone body at the cap fits the least bound with
// its room; a body that stops arriving gives its room back with its bytes.
func Test_BodyMeter_DecodeRoomByReader(t *testing.T) {
	SetMaxBody(DefaultMaxBody)
	withBound(t, 1, 100*time.Millisecond)

	bound, _ := InFlight()

	whole, ctx := meteredBody(t, int(DefaultMaxBody), DefaultMaxBody)
	_, err := ReadBody(whole, DefaultMaxBody)
	require.NoError(t, err, "a lone body at the cap fits with its decode room")

	_, held := InFlight()
	assert.Equal(t, DefaultMaxBody+bound-(DefaultMaxBody+1), held,
		"its room stops at the bound less one body's share")

	ReleaseBody(ctx)

	const size = 4 << 20

	form, formCtx := streamBody(t, size, size)
	armBodyFor(formCtx, FormDecode)
	_, err = ReadBody(form, size)
	require.NoError(t, err)

	_, held = InFlight()
	assert.Equal(t, int64(size), held, "a form plans no decode room")

	ReleaseBody(formCtx)

	SetBodyIdle(time.Minute)
	t.Cleanup(func() { SetBodyIdle(DefaultBodyIdle) })

	stalled, stalledCtx := streamBody(t, 0, size)
	stalled.body = io.NopCloser(io.MultiReader(bytes.NewReader(make([]byte, 2*FreeBody)),
		iotest.ErrReader(os.ErrDeadlineExceeded)))
	stalled.IdleBy(func(time.Time) error { return nil })
	ArmBody(stalledCtx)

	_, err = ReadBody(stalled, size)
	require.ErrorIs(t, err, ErrBodyIdle)

	_, held = InFlight()
	assert.Zero(t, held, "a body that stopped arriving gives back its bytes and its decode room")
}

// Test_BodyMeter_PlansTheServicesElementCost: a service that declares what one
// element costs (Charge.PlanUnit) has its bodies admitted with the room their
// elements would need at the density of their first FreeBody bytes. A body
// with no elements is planned as any other json body, a dense one up to what a
// request may be charged, a service that declares nothing is planned as before,
// and a body whose length is not stated is not sampled.
func Test_BodyMeter_PlansTheServicesElementCost(t *testing.T) {
	SetMaxBody(DefaultMaxBody)
	withBound(t, DefaultInFlight, 100*time.Millisecond)

	const size = 1 << 20

	bound, _ := InFlight()
	room := bound - (DefaultMaxBody + 1)

	held := func(content []byte, length, unit int64) int64 {
		c := new(Charge)
		ctx := WithCharge(t.Context(), c)
		m := NewBodyMeter(ctx, io.NopCloser(bytes.NewReader(content)), length)
		require.NotNil(t, m)
		c.Meter(m)
		c.PlanUnit(unit)
		ArmBody(ctx)

		_, err := ReadBody(m, length)
		require.NoError(t, err)

		_, used := InFlight()

		ReleaseBody(ctx)

		return used
	}

	sparse := bytes.Repeat([]byte("a"), size)
	dense := bytes.Repeat([]byte(`{"N":"1"},`), size/10)
	dense = append(dense, sparse[:size-len(dense)]...)
	// FreeBody bytes of text, then elements: the sample sees only the text.
	late := append(bytes.Repeat([]byte("a"), FreeBody), dense[:size-FreeBody]...)

	const unit = 1700

	plain := held(sparse, size, 0)
	assert.Equal(t, plain, held(dense, size, 0), "a service that declares no unit is planned as before")
	assert.Equal(t, plain, held(sparse, size, unit), "a body with no elements is planned as before")
	assert.Equal(t, plain, held(late, size, unit), "only the first FreeBody bytes are sampled")
	assert.Equal(t, int64(size)+room, held(dense, size, unit), "a dense body is planned up to what a request may hold")
	assert.Equal(t, int64(size), held(dense, -1, unit), "a body of unknown length is charged what it read, not sampled")

	// Between: one element in 100 bytes costs 1,700 bytes of 100.
	between := bytes.Repeat(append(bytes.Repeat([]byte("a"), 99), ','), size/100)
	got := held(between, size, unit)
	assert.Greater(t, got, plain)
	assert.Less(t, got, int64(size)+room)
}

// Test_BodyMeter_UnknownLengthKeepsWhatItRead: a body of unknown length a few
// bytes past a doubling step is charged twice what it read while it is read,
// as ReadBody's buffers are, and only what it read once it ends.
func Test_BodyMeter_UnknownLengthKeepsWhatItRead(t *testing.T) {
	SetMaxBody(DefaultMaxBody)
	withBound(t, 1, 100*time.Millisecond)

	const size = 1<<20 + 72

	m, ctx := meteredBody(t, size, -1)
	body, err := ReadBody(m, -1)
	require.NoError(t, err)
	require.Len(t, body, size)

	_, held := InFlight()
	assert.Equal(t, int64(size), held, "the body holds what it read, not the 2 MiB step it reached")

	ReleaseBody(ctx)

	_, held = InFlight()
	assert.Zero(t, held)
}

// Test_BodyMeter_IdleDeadlineStaysPast: a body whose client stopped sending
// fails with ErrBodyIdle, and its meter leaves the passed read deadline in
// place, so the server's read of what is left of the body fails at once and
// the answer is sent; a body read to its end clears its deadline.
func Test_BodyMeter_IdleDeadlineStaysPast(t *testing.T) {
	withBound(t, DefaultInFlight, time.Second)
	SetBodyIdle(time.Minute)
	t.Cleanup(func() { SetBodyIdle(DefaultBodyIdle) })

	var cleared int

	set := func(d time.Time) error {
		if d.IsZero() {
			cleared++
		}

		return nil
	}

	c := new(Charge)
	ctx := WithCharge(t.Context(), c)
	stalled := NewBodyMeter(ctx, io.NopCloser(iotest.ErrReader(os.ErrDeadlineExceeded)), 1<<20)
	c.Meter(stalled)
	stalled.IdleBy(set)
	ArmBody(ctx)

	_, err := stalled.Read(make([]byte, 16))
	require.ErrorIs(t, err, ErrBodyIdle)

	c.Release()
	assert.Zero(t, cleared, "the passed deadline is left in place")

	whole, wholeCtx := streamBody(t, 1<<20, 1<<20)
	whole.IdleBy(set)
	ArmBody(wholeCtx)

	_, err = ReadBody(whole, 1<<20)
	require.NoError(t, err)
	assert.Equal(t, 1, cleared, "a body read to its end clears its deadline")

	whole.Release()
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

// Test_ReadBody_ReadsEveryShape: a body of any size arrives whole, whatever
// length it states and however its reader hands it over, a body over the cap
// reads one byte past it, and an honest body allocates about twice its length.
func Test_ReadBody_ReadsEveryShape(t *testing.T) {
	SetMaxBody(4 << 20)
	t.Cleanup(func() { SetMaxBody(DefaultMaxBody) })

	sizes := []int{0, 1, FreeBody - 1, FreeBody, FreeBody + 1, 3*FreeBody + 7, 1 << 20, 4 << 20, 4<<20 + 1, 5 << 20}
	stated := map[string]func(n int) int64{
		"honest":      func(n int) int64 { return int64(n) },
		"unstated":    func(int) int64 { return -1 },
		"states less": func(n int) int64 { return int64(n / 2) },
		"states more": func(n int) int64 { return int64(2*n + 1) },
	}
	readers := map[string]func(io.Reader) io.Reader{
		"whole":    func(r io.Reader) io.Reader { return r },
		"one byte": iotest.OneByteReader,
		"half":     iotest.HalfReader,
	}

	for _, size := range sizes {
		want := make([]byte, size)
		for i := range want {
			want[i] = byte(i * 7)
		}

		for sname, length := range stated {
			for rname, wrap := range readers {
				if rname == "one byte" && size > 1<<20 {
					continue // the same paths as at 1 MiB, much slower
				}

				t.Run(fmt.Sprintf("%d/%s/%s", size, sname, rname), func(t *testing.T) {
					got, err := ReadBody(wrap(bytes.NewReader(want)), length(size))
					require.NoError(t, err)

					text, err := ReadBodyText(wrap(bytes.NewReader(want)), length(size))
					require.NoError(t, err)
					assert.Equal(t, string(got), text, "ReadBodyText reads what ReadBody does")

					if OverBody(size) {
						assert.Len(t, got, 4<<20+1, "a body over the cap reads one byte past it")
						assert.True(t, OverBody(len(got)))

						return
					}

					assert.Equal(t, want, got)
				})
			}
		}
	}

	t.Run("an honest body allocates about twice its length", func(t *testing.T) {
		body := make([]byte, 3<<20)

		var before, after runtime.MemStats

		runtime.ReadMemStats(&before)

		got, err := ReadBody(bytes.NewReader(body), int64(len(body)))

		runtime.ReadMemStats(&after)
		require.NoError(t, err)
		require.Len(t, got, len(body))
		assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(2*len(body)+2*FreeBody))
	})

	t.Run("the text of an honest body costs no copy past ReadBody's", func(t *testing.T) {
		body := make([]byte, 3<<20)

		var before, after runtime.MemStats

		runtime.ReadMemStats(&before)

		got, err := ReadBodyText(bytes.NewReader(body), int64(len(body)))

		runtime.ReadMemStats(&after)
		require.NoError(t, err)
		require.Len(t, got, len(body))
		assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(2*len(body)+2*FreeBody))
	})
}
