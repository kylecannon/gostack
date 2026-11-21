package protocol

import (
	"math"
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeHeap makes ReserveHeap read objects and live as the heap and count the
// collections it runs, which leave live as the objects; it puts the runtime's
// back after the test.
func fakeHeap(t *testing.T, objects, live int64) *int {
	t.Helper()

	read, collect, limit := heap.read, heap.collect, debug.SetMemoryLimit(-1)
	collections := 0

	heap.read = func() (int64, int64) { return objects, live }
	heap.collect = func() {
		collections++
		objects = live
	}

	t.Cleanup(func() {
		heap.read, heap.collect = read, collect
		debug.SetMemoryLimit(limit)
	})

	return &collections
}

// Test_ReserveHeap_WeighsTheHeapAndWhatIsReservedAgainstTheLimit: with no Go
// memory limit nothing is reserved; with one, an estimate is reserved while the
// heap's objects, what is reserved and it fit the limit less the in-flight
// bound and HeapSlack, and given back by its release.
func Test_ReserveHeap_WeighsTheHeapAndWhatIsReservedAgainstTheLimit(t *testing.T) {
	const objects = 20 << 20

	collections := fakeHeap(t, objects, objects)
	bound, _ := InFlight()
	if bound == 0 {
		bound = DefaultInFlight
	}

	debug.SetMemoryLimit(math.MaxInt64)

	_, ok := ReserveHeap(1)
	assert.False(t, ok, "no limit: nothing is proven to fit")

	room := int64(100 << 20)
	debug.SetMemoryLimit(bound + HeapSlack + objects + room)

	first, ok := ReserveHeap(room / 2)
	require.True(t, ok, "half the room")

	second, ok := ReserveHeap(room / 2)
	require.True(t, ok, "the other half")

	_, ok = ReserveHeap(1)
	assert.False(t, ok, "a byte past what the two reserved")

	first()

	_, ok = ReserveHeap(room/2 + 1)
	assert.False(t, ok, "a byte more than the first gave back")

	third, ok := ReserveHeap(room / 2)
	assert.True(t, ok, "what the first gave back")

	second()
	third()

	assert.Zero(t, heap.reserved, "every release gives back its reservation")
	assert.Zero(t, *collections, "the heap was all live: no collection could make room")
}

// Test_ReserveHeap_CollectsWhenOnlyGarbageStandsInTheWay: objects the last
// collection did not find live count until a collection frees them, which
// ReserveHeap runs only when that would make room.
func Test_ReserveHeap_CollectsWhenOnlyGarbageStandsInTheWay(t *testing.T) {
	const live = 20 << 20

	collections := fakeHeap(t, live+300<<20, live)
	bound, _ := InFlight()
	if bound == 0 {
		bound = DefaultInFlight
	}

	debug.SetMemoryLimit(bound + HeapSlack + live + 100<<20)

	_, ok := ReserveHeap(200 << 20)
	assert.False(t, ok, "more than a collection would free")
	assert.Zero(t, *collections)

	release, ok := ReserveHeap(100 << 20)
	require.True(t, ok, "what the collection freed")
	assert.Equal(t, 1, *collections)

	release()
}

// Test_Charge_KeepsTheHeapUntilTheRequestIsReleased: a reservation kept by a
// request's Charge stays reserved after decoding, while the handler works on
// the value, and is given back when the gateway releases the request.
func Test_Charge_KeepsTheHeapUntilTheRequestIsReleased(t *testing.T) {
	const objects = 20 << 20

	fakeHeap(t, objects, objects)
	bound, _ := InFlight()
	if bound == 0 {
		bound = DefaultInFlight
	}

	debug.SetMemoryLimit(bound + HeapSlack + objects + 100<<20)

	var c Charge

	var keeper HeapKeeper = &c

	for range 2 {
		release, ok := ReserveHeap(40 << 20)
		require.True(t, ok)
		keeper.KeepHeap(release)
	}

	assert.Equal(t, int64(80<<20), heap.reserved, "both stay reserved past decoding")

	_, ok := ReserveHeap(40 << 20)
	assert.False(t, ok, "what the request keeps is not room for another")

	c.Release()

	assert.Zero(t, heap.reserved, "the request's release gives both back")
}
