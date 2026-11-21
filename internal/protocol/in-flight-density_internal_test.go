package protocol

import (
	"bytes"
	"context"
	"io"
	"math"
	"runtime/debug"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_BodyMeter_SampleCountsStructureNotText: the sample counts the commas and
// opening brackets that are json structure and not those inside a string, as the
// reads split the body anywhere, between a backslash and what it escapes
// included.
func Test_BodyMeter_SampleCountsStructureNotText(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int64
	}{
		{"a list", `[1,2,3]`, 3},
		{"a comma in a string", `"a,b,[c"`, 0},
		{"separators after a string", `"a,b",[1,2]`, 3},
		{"an escaped quote does not end the string", `"a\",b,[c"`, 0},
		{"an escaped backslash does not escape the quote after it", `"a\\",b`, 1},
		{"escapes in a string of separators", `"\\\\,\",\\",[`, 2},
		{"members of an object", `{"a":"x,y","b":{"c":[1]}}`, 2},
		{"text with no quote", `a,b,[`, 3},
		{"a string that does not end", `[1,"a,b,c`, 2},
	}

	for _, tt := range tests {
		for _, chunk := range []int{1, 2, 3, 5, len(tt.body)} {
			t.Run(tt.name, func(t *testing.T) {
				m := &BodyMeter{}

				for rest := []byte(tt.body); len(rest) > 0; {
					n := min(chunk, len(rest))
					m.sample(rest[:n])
					rest = rest[n:]
				}

				assert.Equal(t, tt.want, m.elements, "chunks of %d bytes", chunk)
				assert.Equal(t, int64(len(tt.body)), m.sampled)
			})
		}
	}
}

// Test_BodyMeter_StringOfSeparatorsIsPlannedByLength: a json string of comma
// separated values is planned twice its length, as a body of strings is: the
// commas in it are text. The same bytes outside a string, 65,537 of them with a
// comma every 4, are planned what 16,000 elements at the unit would need. Only
// the first FreeBody bytes are sampled, so a body is planned by its first
// FreeBody bytes, and the room it was not planned for is taken as it decodes.
func Test_BodyMeter_StringOfSeparatorsIsPlannedByLength(t *testing.T) {
	SetMaxBody(DefaultMaxBody)
	withBound(t, DefaultInFlight, 100*time.Millisecond)

	const unit = 1700

	held := func(size int) int64 {
		return heldBy(t, func(n int) []byte {
			csv := bytes.Repeat([]byte("123,"), n/4+1)

			return csv[:n]
		}, size, unit)
	}

	quoted := func(size int) int64 {
		return heldBy(t, func(n int) []byte {
			csv := bytes.Repeat([]byte("123,"), n/4+1)
			csv = append([]byte{'"'}, csv[:n-2]...)

			return append(csv, '"')
		}, size, unit)
	}

	for _, size := range []int{FreeBody + 1, 100_000, 400_000} {
		assert.Equal(t, int64(size), quoted(size), "a string of %d bytes is planned no more than its body", size)
		assert.Greater(t, held(size), int64(size)+8<<20, "the same %d bytes as structure are planned by elements", size)
	}

	// A body of 1 MiB + 1 B of a string is planned the 2x of its text past
	// FreeDecode, as before this unit existed.
	size := 1<<20 + 1
	assert.Equal(t, int64(size)+(int64(2*size)+FreeBody-FreeDecode+decodeChunk-1)/decodeChunk*decodeChunk,
		quoted(size), "planned by length: twice it, and a free allowance")
}

// heldBy returns what the pool charges for a json body of size bytes that make
// returns, for a service that declares unit, once it is read whole.
func heldBy(t *testing.T, build func(n int) []byte, size int, unit int64) int64 {
	t.Helper()

	c := new(Charge)
	ctx := WithCharge(t.Context(), c)
	m := NewBodyMeter(ctx, io.NopCloser(bytes.NewReader(build(size))), int64(size))
	require.NotNil(t, m)
	c.Meter(m)
	c.PlanUnit(unit)
	ArmBody(ctx)

	_, err := ReadBody(m, int64(size))
	require.NoError(t, err)

	_, used := InFlight()

	ReleaseBody(ctx)

	return used
}

// Test_bodyPool_RegrowWaitsHoldingNothing: two holders each hold part of the
// pool and each wants more than the pool has left. A holder that waited for the
// difference while keeping what it held would wait on the other, who waits on
// it, until the wait ended; regrow gives back what a holder held when it has to
// wait, so the first is served at once and the second when it is done.
func Test_bodyPool_RegrowWaitsHoldingNothing(t *testing.T) {
	SetBodyWait(5 * time.Second)
	t.Cleanup(func() { SetBodyWait(DefaultBodyWait) })

	pool := &bodyPool{capacity: 10}

	require.NoError(t, pool.acquire(t.Context(), 4, &pool.starting))
	require.NoError(t, pool.acquire(t.Context(), 4, &pool.starting))

	// Each of them wants 7: 3 more, and 2 are free.
	var wg sync.WaitGroup

	granted := make(chan string, 2)

	for _, name := range []string{"first", "second"} {
		wg.Go(func() {
			if !assert.NoError(t, pool.regrow(context.Background(), 4, 7), name) {
				return
			}

			granted <- name
		})

		// The first one queues before the second.
		for name == "first" && !queued(pool, &pool.growing) {
			time.Sleep(time.Millisecond)
		}
	}

	first := <-granted
	assert.Equal(t, "first", first, "in the order they waited")

	held := inUse(pool)
	assert.Equal(t, int64(7), held, "the one served holds 7 and the other holds nothing while it waits")

	pool.release(7)

	assert.Equal(t, "second", <-granted)

	pool.release(7)
	wg.Wait()

	held = inUse(pool)
	assert.Zero(t, held)
}

// Test_bodyPool_RegrowTakesTheDifferenceWhenItFits: a holder that finds room for
// the difference keeps what it held and takes only that.
func Test_bodyPool_RegrowTakesTheDifferenceWhenItFits(t *testing.T) {
	pool := &bodyPool{capacity: 10}

	require.NoError(t, pool.acquire(t.Context(), 4, &pool.starting))
	require.NoError(t, pool.regrow(t.Context(), 4, 9))

	held := inUse(pool)
	assert.Equal(t, int64(9), held)
}

// Test_bodyPool_RegrowThatFailsHoldsNothing: a holder that waited and was
// refused has given back what it held, so the pool is as if it had not been
// there.
func Test_bodyPool_RegrowThatFailsHoldsNothing(t *testing.T) {
	SetBodyWait(20 * time.Millisecond)
	t.Cleanup(func() { SetBodyWait(DefaultBodyWait) })

	pool := &bodyPool{capacity: 10}

	require.NoError(t, pool.acquire(t.Context(), 6, &pool.starting))
	require.NoError(t, pool.acquire(t.Context(), 3, &pool.starting))

	require.ErrorIs(t, pool.regrow(t.Context(), 3, 8), ErrBusy)

	held := inUse(pool)
	assert.Equal(t, int64(6), held, "the 3 it held are back")
	assert.Empty(t, pool.growing)
}

func inUse(pool *bodyPool) int64 {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	return pool.used
}

// Test_DecodeHold_PlannedTooLittleWaitsHoldingOnlyItsBody: a body planned the
// room of a body of strings that then decodes dense (its density was not in its
// first FreeBody bytes) gives its planned room back when it has to wait for the
// rest, and holds only its body until it is served.
func Test_DecodeHold_PlannedTooLittleWaitsHoldingOnlyItsBody(t *testing.T) {
	SetMaxBody(DefaultMaxBody)
	withBound(t, 64<<20, 100*time.Millisecond)

	pool := inFlight.Load()
	require.NoError(t, pool.acquire(t.Context(), 40<<20, &pool.starting), "other requests hold most of the pool")

	var hold DecodeHold

	require.NoError(t, pool.acquire(t.Context(), 2<<20+1<<20, &pool.starting))
	hold.prepaid(pool, 2<<20)
	require.EqualValues(t, 2<<20, hold.held)

	// 1 MiB is the body's. It wants 30 MiB of decoding and 21 MiB are free.
	_, err := hold.Cover(t.Context(), 30<<20)
	require.ErrorIs(t, err, ErrBusy)
	assert.Zero(t, hold.held, "what was planned went back when it waited")

	used := inUse(pool)
	assert.Equal(t, int64(41<<20), used, "its body's 1 MiB and the others' 40 MiB")

	_, err = hold.Cover(t.Context(), 30<<20)
	require.ErrorIs(t, err, ErrBusy, "and it fails the same way after")
}

// Test_DecodeHold_PastTheMemoryLimitFailsWithErrOverMemory: with a Go memory
// limit set, a decode that would keep more than half of it live fails at once,
// and with ErrBusy as well for a caller that does not tell them apart; with no
// limit, or one it fits, the hold is charged as before.
func Test_DecodeHold_PastTheMemoryLimitFailsWithErrOverMemory(t *testing.T) {
	SetMaxBody(DefaultMaxBody)
	withBound(t, DefaultInFlight, 100*time.Millisecond)

	const cost = 400 << 20 // about 100 MiB of heap

	old := debug.SetMemoryLimit(-1)

	t.Cleanup(func() { debug.SetMemoryLimit(old) })

	for _, tt := range []struct {
		name    string
		limit   int64
		refused bool
	}{
		{"no limit", math.MaxInt64, false},
		{"a limit that half of holds it", 256 << 20, false},
		{"a limit that half of does not", 150 << 20, true},
	} {
		debug.SetMemoryLimit(tt.limit)

		var hold DecodeHold

		_, err := hold.Cover(t.Context(), cost)

		if !tt.refused {
			require.NoError(t, err, tt.name)
			hold.Release()

			continue
		}

		require.ErrorIs(t, err, ErrOverMemory, tt.name)
		require.ErrorIs(t, err, ErrBusy, tt.name)

		_, err = hold.Cover(t.Context(), 2<<20)
		require.ErrorIs(t, err, ErrOverMemory, "and it fails the same way after")
		hold.Release()
	}

	_, held := InFlight()
	assert.Zero(t, held)
}
