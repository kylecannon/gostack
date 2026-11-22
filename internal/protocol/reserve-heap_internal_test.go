package protocol

import (
	"context"
	"math"
	"runtime/debug"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeHeap makes ReserveHeap read objects and live as the heap and count the
// collections it runs, which leave live as the objects; it puts the runtime's
// back after the test.
func fakeHeap(t *testing.T, objects, live int64) *int {
	t.Helper()

	read, collect, limit, memoryCap := heap.read, heap.collect, debug.SetMemoryLimit(-1), SetMemoryCap(0)
	collections := 0

	heap.read = func() (int64, int64) { return objects, live }
	heap.collect = func() {
		collections++
		objects = live
	}

	t.Cleanup(func() {
		heap.read, heap.collect = read, collect
		debug.SetMemoryLimit(limit)
		SetMemoryCap(memoryCap)
	})

	return &collections
}

// Test_ReserveHeap_WeighsTheHeapAndWhatIsReservedAgainstTheLimit: an estimate
// is reserved while the heap's objects, what is reserved and it fit the Go
// memory limit less the in-flight bound and HeapSlack, and given back by its
// release. With no Go memory limit and no memory cap, the limit weighed is
// SoftLimit(UnknownMemory): a body that fits it is reserved, one past it is
// not.
func Test_ReserveHeap_WeighsTheHeapAndWhatIsReservedAgainstTheLimit(t *testing.T) {
	const objects = 20 << 20

	collections := fakeHeap(t, objects, objects)
	bound, _ := InFlight()
	if bound == 0 {
		bound = DefaultInFlight
	}

	debug.SetMemoryLimit(math.MaxInt64)

	unknown := SoftLimit(UnknownMemory) - bound - HeapSlack - objects

	_, ok := ReserveHeap(unknown + 1)
	assert.False(t, ok, "no limit: a byte past the room of the unknown memory")

	release, ok := ReserveHeap(unknown)
	require.True(t, ok, "no limit: the room of the unknown memory")
	release()

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
	assert.Equal(t, 3, *collections, "each refusal a collection could change collected first, the heap being all live")
}

// Test_ReserveHeap_TheMemoryCapBoundsTheLimit: a memory cap below the Go
// memory limit, a container's under a GOMEMLIMIT set above it, is the limit
// ReserveHeap weighs against; one above it changes nothing; with no Go memory
// limit, the cap is the limit.
func Test_ReserveHeap_TheMemoryCapBoundsTheLimit(t *testing.T) {
	const objects = 20 << 20

	fakeHeap(t, objects, objects)
	bound, _ := InFlight()
	if bound == 0 {
		bound = DefaultInFlight
	}

	container := int64(512 << 20 / 100 * 85)
	room := container - bound - HeapSlack - objects

	for _, limit := range []int64{4 << 30, math.MaxInt64} {
		debug.SetMemoryLimit(limit)
		SetMemoryCap(container)

		_, ok := ReserveHeap(room + 1)
		assert.False(t, ok, "limit %d: a byte past the cap's room", limit)

		release, ok := ReserveHeap(room)
		require.True(t, ok, "limit %d: the cap's room", limit)
		release()
	}

	debug.SetMemoryLimit(container - 100<<20)
	SetMemoryCap(container)

	_, ok := ReserveHeap(room - 100<<20 + 1)
	assert.False(t, ok, "a limit under the cap is the limit")

	assert.Zero(t, heap.reserved)
}

// Test_ReserveHeap_CollectsWhenOnlyGarbageStandsInTheWay: objects the last
// collection did not find live count until a collection frees them, which
// ReserveHeap runs only when it could make room: when what is reserved and
// the estimate fit the room alone.
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

// Test_TakeDenseLane_OneBodyAtATimeAndNoneWaits: the lane is taken with a
// heap reservation and refused at once while it is held; a body the heap has
// no room for leaves the lane free; the lane's release runs once, so that a
// second call cannot free the lane another body has taken since.
func Test_TakeDenseLane_OneBodyAtATimeAndNoneWaits(t *testing.T) {
	const objects = 20 << 20

	fakeHeap(t, objects, objects)

	bound, _ := InFlight()
	if bound == 0 {
		bound = DefaultInFlight
	}

	room := int64(100 << 20)
	debug.SetMemoryLimit(bound + HeapSlack + objects + room)

	lane, release, err := TakeDenseLane(room/4, room/4)
	require.NoError(t, err, "the lane is free and the heap has room")
	assert.Equal(t, room/4, heap.reserved)

	_, _, err = TakeDenseLane(1, 1)
	require.ErrorIs(t, err, ErrDenseLaneTaken)
	assert.Equal(t, room/4, heap.reserved, "a refused body reserves nothing")

	lane()
	release()
	assert.Zero(t, heap.reserved)
	assert.False(t, denseLane.Load(), "the release frees the lane")

	_, _, err = TakeDenseLane(room+1, room+1)
	require.ErrorIs(t, err, ErrNoHeapRoom)
	assert.False(t, denseLane.Load(), "a body with no room leaves the lane free")

	nextLane, nextRelease, err := TakeDenseLane(1, 1)
	require.NoError(t, err)

	lane()
	assert.True(t, denseLane.Load(), "a second release of the lane frees nothing")

	nextLane()
	nextRelease()
	assert.False(t, denseLane.Load())
	assert.Zero(t, heap.reserved)
}

// Test_HeapCouldFit_WeighsWhatIsLiveAndReserved: HeapCouldFit admits what
// ReserveHeap could reserve after a collection, from what is live and what is
// reserved, reserving nothing and collecting nothing; with no Go memory limit
// it weighs them against the room of SoftLimit(UnknownMemory).
func Test_HeapCouldFit_WeighsWhatIsLiveAndReserved(t *testing.T) {
	collections := fakeHeap(t, 80<<20, 20<<20)

	bound, _ := InFlight()
	if bound == 0 {
		bound = DefaultInFlight
	}

	debug.SetMemoryLimit(math.MaxInt64)

	unknown := SoftLimit(UnknownMemory) - bound - HeapSlack - 20<<20
	assert.True(t, HeapCouldFit(unknown), "no limit: the room of the unknown memory")
	assert.False(t, HeapCouldFit(unknown+1), "no limit: a byte past it")

	room := int64(100 << 20)
	debug.SetMemoryLimit(bound + HeapSlack + room)

	assert.True(t, HeapCouldFit(room-20<<20), "garbage does not count")
	assert.False(t, HeapCouldFit(room-20<<20+1))

	release, ok := ReserveHeap(30 << 20)
	require.True(t, ok)
	assert.True(t, HeapCouldFit(room-50<<20), "what is reserved counts")
	assert.False(t, HeapCouldFit(room-50<<20+1))
	release()

	assert.Equal(t, 3, *collections, "each refusal a collection could have changed collected first; nothing else did")
	assert.Zero(t, heap.reserved)
}

// Test_HeapCouldFit_CollectsBeforeItRefuses: when what the last collection
// found live leaves no room, HeapCouldFit collects and reads again, so a
// value that died since, an earlier request's, does not refuse the body. What
// a collection leaves live still refuses it. A body past the room with nothing
// live is refused without a collection (HeapRoomFor).
func Test_HeapCouldFit_CollectsBeforeItRefuses(t *testing.T) {
	collections := fakeHeap(t, 0, 0)

	bound, _ := InFlight()
	if bound == 0 {
		bound = DefaultInFlight
	}

	room := int64(100 << 20)
	debug.SetMemoryLimit(bound + HeapSlack + room)

	// The last collection ran while 80 MiB of an earlier value were live; a
	// collection now leaves 20 MiB.
	live, after := int64(80<<20), int64(20<<20)
	heap.read = func() (int64, int64) { return live, live }
	heap.collect = func() {
		*collections++
		live = after
	}

	assert.True(t, HeapCouldFit(room-after), "the earlier value is garbage")
	assert.Equal(t, 1, *collections)

	assert.False(t, HeapCouldFit(room-after+1), "what is still live after a collection counts")
	assert.Equal(t, 2, *collections)

	assert.False(t, HeapRoomFor(room+1))
	assert.False(t, HeapCouldFit(room+1), "no collection makes room past the room itself")
	assert.Equal(t, 2, *collections, "and none runs")

	assert.True(t, HeapRoomFor(room))
}

// Test_BodiesWait_InEitherQueue:BodiesWait reports a body or a decoding
// waiting for room under the in-flight bound, in either queue, and nothing
// once it is granted; with no bound nothing waits. A waiter that what the
// caller gives back would let in does not count; one it would not does.
func Test_BodiesWait_InEitherQueue(t *testing.T) {
	old, oldWait := inFlight.Load(), bodyWait.Load()

	t.Cleanup(func() {
		inFlight.Store(old)
		bodyWait.Store(oldWait)
	})

	SetInFlight(0)
	assert.False(t, BodiesWait(0), "no bound")

	SetInFlight(DefaultInFlight)
	SetBodyWait(time.Minute)

	pool := inFlight.Load()

	for _, queue := range []*[]*bodyWaiter{&pool.starting, &pool.growing} {
		require.NoError(t, pool.acquire(context.Background(), pool.capacity, queue))

		waited := make(chan error, 1)

		go func() { waited <- pool.acquire(context.Background(), 1, queue) }()

		require.Eventually(t, func() bool {
			pool.mu.Lock()
			defer pool.mu.Unlock()

			return len(*queue) == 1
		}, 10*time.Second, time.Millisecond)

		assert.True(t, BodiesWait(0), "a body waits for the bound")
		assert.False(t, BodiesWait(pool.capacity), "a body waits only on what the caller holds")
		assert.False(t, BodiesWait(1), "a body waits only on the caller's byte")

		pool.release(pool.capacity)
		require.NoError(t, <-waited)
		assert.False(t, BodiesWait(0), "the waiter is granted")
		pool.release(1)
	}

	// Two waiters in order, of which what the caller holds lets in only the
	// first.
	require.NoError(t, pool.acquire(context.Background(), pool.capacity, &pool.starting))

	waited := make(chan error, 2)

	for range 2 {
		go func() { waited <- pool.acquire(context.Background(), pool.capacity/2, &pool.starting) }()
	}

	require.Eventually(t, func() bool {
		pool.mu.Lock()
		defer pool.mu.Unlock()

		return len(pool.starting) == 2
	}, 10*time.Second, time.Millisecond)

	assert.True(t, BodiesWait(pool.capacity/2), "the second waits past what the caller gives back")
	assert.False(t, BodiesWait(pool.capacity), "both are let in")

	pool.release(pool.capacity)
	require.NoError(t, <-waited)
	require.NoError(t, <-waited)
	pool.release(pool.capacity / 2 * 2)
}

// Test_TakeDenseLane_TheLaneIsFreeWhileTheRequestIsAnswered: once a dense
// body is decoded its lane is given back, while its reservation stays on its
// request's Charge until the request is answered; a second dense body takes
// the lane while the first is still being answered, and the first's
// reservation counts against it.
func Test_TakeDenseLane_TheLaneIsFreeWhileTheRequestIsAnswered(t *testing.T) {
	fakeHeap(t, 0, 0)

	bound, _ := InFlight()
	if bound == 0 {
		bound = DefaultInFlight
	}

	room := int64(100 << 20)
	debug.SetMemoryLimit(bound + HeapSlack + room)

	first := &Charge{}

	lane, release, err := TakeDenseLane(room/2, room/2)
	require.NoError(t, err)
	first.KeepHeap(release)
	lane() // decoded: the response is being written

	assert.False(t, denseLane.Load(), "the lane is free once decoded")
	assert.Equal(t, room/2, heap.reserved, "the reservation is still held")

	_, _, err = TakeDenseLane(room/2+1, room/2+1)
	require.ErrorIs(t, err, ErrNoHeapRoom, "the first body's reservation still counts")
	assert.False(t, denseLane.Load())

	second := &Charge{}

	lane, release, err = TakeDenseLane(room/2, room/2)
	require.NoError(t, err, "a second body takes the lane while the first is answered")
	second.KeepHeap(release)
	lane()

	first.Release()
	assert.Equal(t, room/2, heap.reserved)

	second.Release()
	assert.Zero(t, heap.reserved)
	assert.False(t, denseLane.Load())
}
