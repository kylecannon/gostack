package awsjson

import (
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/heaptest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lightThenDense is a denseValue list whose first element is a string of p
// bytes and whose other d-1 elements are empty: light where the walk projects
// its heap from, dense after.
func lightThenDense(p, d int) []byte {
	return []byte(`{"L":[{"S":"` + strings.Repeat("x", p) + `"},` + strings.Repeat(`{},`, d-1) + `{}]}`)
}

// projectedAndFinal is what moveBody projects for body where the walk passes
// protocol.DecodeBudget, its bytes and the projected heap, and what
// chargeDecode reserves at the end of the walk.
func projectedAndFinal(body []byte, budget int64) (projected, final int64) {
	p := planOf(reflect.TypeFor[denseValue]())
	w := walker{dec: newWalkDecoder(body), limit: budget / decodePasses}
	w.mark = protocol.DecodeBudget(len(body)) / decodePasses
	w.move = func(heap, read int64) error {
		projected = int64(len(body)) + projectHeap(heap, read, len(body))

		return nil
	}

	_, heap, _ := w.walk(p)

	return projected, heap + allocSize(p.heapSize)
}

// liveAfterCollection is what a collection now leaves live.
func liveAfterCollection() int64 {
	runtime.GC() //nolint:revive // the test weighs what a collection leaves

	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)

	return int64(stats.HeapAlloc)
}

// requireNothingReserved fails unless the lane is free and the whole room can
// be reserved.
func requireNothingReserved(t *testing.T, room int64) {
	t.Helper()

	requireLaneFree(t)
	require.NoError(t, protocol.CheckHeapRoom(room), "nothing is still reserved")
}

// Test_DecodeWithin_TheFinalReservationAnswersAsTheTable: a 6.7 MiB body light
// at its start and dense after it is projected low where its walk passes
// protocol.DecodeBudget, takes the lane, and is decided by the final
// reservation (SQ-508). With room 1 MiB past its bytes and estimate, it is too
// large alone, and too large beside another request's 1-byte reservation,
// since what stays without it, its own bytes among it, leaves no room (S1).
// With room for it alone, it is served alone, throttled beside another
// request's large reservation, and served once that request is answered.
func Test_DecodeWithin_TheFinalReservationAnswersAsTheTable(t *testing.T) {
	heaptest.Settle(t)

	body := lightThenDense(4_000_000, 1_000_000)
	budget := protocol.DecodeBudgetAt(len(body), 256)
	n := int64(len(body))
	projected, final := projectedAndFinal(body, budget)

	t.Logf("body %d bytes, projected %d, final reservation %d", n, projected, final)
	require.Less(t, projected, final/2, "the projection admits what the final reservation decides")

	t.Run("too large alone and beside another byte", func(t *testing.T) {
		room := n + final + 1<<20
		withHeapRoom(t, room)

		alone := &recordingHold{}
		_, _, err := DecodeWithin[denseValue](body, budget, alone)
		alone.release()
		require.Same(t, ErrTooLarge, err, "alone")
		assert.Equal(t, []string{stepKeep, stepReleasePool}, alone.steps, "refused at the final reservation")

		other, rerr := protocol.ReserveHeapBeside(1, 0)
		require.NoError(t, rerr)

		beside := &recordingHold{}
		_, _, err = DecodeWithin[denseValue](body, budget, beside)
		beside.release()
		other()
		assert.Same(t, ErrTooLarge, err, "beside another request's byte")
		assert.Equal(t, []string{stepKeep, stepReleasePool}, beside.steps, "refused at the final reservation")

		requireNothingReserved(t, room)
	})

	t.Run("throttled beside another's reservation, served once it is answered", func(t *testing.T) {
		room := 2 * (n + final)
		withHeapRoom(t, room)

		alone := &recordingHold{}
		_, _, err := DecodeWithin[denseValue](body, budget, alone)
		alone.release()
		require.Nil(t, err, "alone")

		x := room - liveAfterCollection() - projected - 8<<20

		other, rerr := protocol.ReserveHeapBeside(x, 0)
		require.NoError(t, rerr)

		beside := &recordingHold{}
		_, _, err = DecodeWithin[denseValue](body, budget, beside)
		assert.Equal(t, protocol.ThrottledJSON(), err, "beside another request's reservation")
		assert.Equal(t, []string{stepKeep, stepReleasePool}, beside.steps, "throttled at the final reservation")
		requireLaneFree(t)

		beside.release()
		other()

		again := &recordingHold{}
		_, _, err = DecodeWithin[denseValue](body, budget, again)
		again.release()
		assert.Nil(t, err, "served once the other is answered")

		requireNothingReserved(t, room)
	})
}
