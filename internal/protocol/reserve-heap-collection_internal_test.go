package protocol

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests reach the heap only through what the package had before the
// verdict was one function (TakeDenseLane, ReserveHeapBeside, fakeHeap), so
// they run on the earlier code too and fail there.

// collectionMiB is a mebibyte.
const collectionMiB = 1 << 20

// collectionRoom gives the fake heap (fakeHeap) room bytes past the in-flight
// bound and HeapSlack, for one test.
func collectionRoom(t *testing.T, room int64) {
	t.Helper()

	fakeHeap(t, 0, 0)

	bound, _ := InFlight()
	if bound == 0 {
		bound = DefaultInFlight
	}

	debug.SetMemoryLimit(bound + HeapSlack + room)
}

// Test_TakeDenseLane_AnotherAnsweredDuringTheCollectionStaysBusy (M1): Y keeps
// 35 MiB reserved and its 30 MiB value live beside a 30 MiB table. X, which
// projects 60 MiB and fits beside the table alone, misses at its first weigh
// and collects; the collection marks Y's value live, and Y is answered before
// X weighs again. That second weigh finds nothing reserved by others and no
// room, for Y's value alone: X is throttled, not refused as too large, and
// served when sent again.
func Test_TakeDenseLane_AnotherAnsweredDuringTheCollectionStaysBusy(t *testing.T) {
	collectionRoom(t, 100*collectionMiB)

	lane, releaseY, err := TakeDenseLane(35*collectionMiB, 35*collectionMiB)
	require.NoError(t, err)
	lane()

	live := int64(60 * collectionMiB) // the table and Y's value
	collections := 0
	heap.read = func() (int64, int64) { return live, live }
	heap.collect = func() {
		collections++

		releaseY() // Y is answered after the collection marked its value
	}

	_, _, err = TakeDenseLane(collectionMiB, 60*collectionMiB)
	require.ErrorIs(t, err, ErrHeapBusy, "Y's value is what misses: X fits alone")
	assert.Equal(t, 1, collections)

	live = 30 * collectionMiB // the next collection frees Y's value
	heap.collect = func() {}

	lane, release, err := TakeDenseLane(collectionMiB, 60*collectionMiB)
	require.NoError(t, err, "X sent again")
	lane()
	release()

	assert.Zero(t, heap.reserved)
	assert.False(t, denseLane.Load())
}

// Test_ReserveHeapBeside_AnotherAnsweredDuringTheCollectionStaysBusy (M1 at
// the final reservation): the same with the body's own bytes reserved, its
// estimate weighed beside them.
func Test_ReserveHeapBeside_AnotherAnsweredDuringTheCollectionStaysBusy(t *testing.T) {
	collectionRoom(t, 100*collectionMiB)

	own, err := ReserveHeapBeside(collectionMiB, 0)
	require.NoError(t, err)

	releaseY, err := ReserveHeapBeside(35*collectionMiB, 0)
	require.NoError(t, err)

	live := int64(60 * collectionMiB) // the table, Y's value and the body
	heap.read = func() (int64, int64) { return live, live }
	heap.collect = func() { releaseY() }

	_, err = ReserveHeapBeside(45*collectionMiB, collectionMiB)
	require.ErrorIs(t, err, ErrHeapBusy, "Y's value is what misses")

	live = 30 * collectionMiB
	heap.collect = func() {}

	release, err := ReserveHeapBeside(45*collectionMiB, collectionMiB)
	require.NoError(t, err, "sent again")

	release()
	own()

	assert.Zero(t, heap.reserved)
}

// Test_TakeDenseLane_AReservationTakenAndGivenBackDuringTheCollectionIsBusy:
// Z reserves after X's first weigh, its value is marked by X's collection, and
// Z is answered before X weighs again. No weigh of X saw Z's reservation, but
// one was given back during X's attempt, so what the collection found live is
// not what stays: X is throttled, and served when sent again.
func Test_TakeDenseLane_AReservationTakenAndGivenBackDuringTheCollectionIsBusy(t *testing.T) {
	collectionRoom(t, 100*collectionMiB)

	live := int64(60 * collectionMiB) // the table, and an earlier value the last collection found
	heap.read = func() (int64, int64) { return live, live }
	heap.collect = func() {
		heap.collect = func() {}

		releaseZ, err := ReserveHeapBeside(30*collectionMiB, 0)
		if err != nil {
			t.Errorf("Z: %v", err)

			return
		}

		releaseZ() // Z's value is marked live, then Z is answered
	}

	_, _, err := TakeDenseLane(collectionMiB, 60*collectionMiB)
	require.ErrorIs(t, err, ErrHeapBusy, "Z's value is what misses")

	live = 30 * collectionMiB

	lane, release, err := TakeDenseLane(collectionMiB, 60*collectionMiB)
	require.NoError(t, err, "X sent again")
	lane()
	release()

	assert.Zero(t, heap.reserved)
}
