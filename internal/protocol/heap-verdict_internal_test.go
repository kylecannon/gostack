package protocol

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// verdictMiB is a mebibyte, the unit the verdict tests weigh in.
const verdictMiB = 1 << 20

// Test_heapVerdict_Table: the one verdict every heap weigh gives, one case per
// row of docs/architecture.md's table, then the sticky-busy sequence and a
// floor that cannot be shown. A body that fits the room now is admitted. One
// that misses is too large when no other request held a reservation during
// the attempt, or when it misses beside the floor of what stays live without
// them; otherwise it is throttled.
func Test_heapVerdict_Table(t *testing.T) {
	const m = verdictMiB

	const room = 100 * verdictMiB

	cases := []struct {
		name string
		w    heapWeigh
		want error
	}{
		{
			name: "row 1: fits the room now, beside others",
			w:    heapWeigh{n: 40 * m, own: 10 * m, reserved: 30 * m, counted: 30 * m, room: room, othersHeld: true},
		},
		{
			name: "row 1: fits the room exactly, alone",
			w:    heapWeigh{n: 60 * m, own: 10 * m, reserved: 10 * m, counted: 30 * m, room: room},
		},
		{
			name: "row 2: misses with no other reservation at any weigh",
			w:    heapWeigh{n: 61 * m, own: 10 * m, reserved: 10 * m, counted: 30 * m, room: room},
			want: ErrNoHeapRoom,
		},
		{
			name: "row 2: a floor that would fit does not matter with no others",
			w:    heapWeigh{n: 61 * m, reserved: 0, counted: 40 * m, room: room},
			want: ErrNoHeapRoom,
		},
		{
			name: "row 3: misses the floor, a table beside another byte (S1, site 2)",
			w: heapWeigh{
				n: 50*m + 1, reserved: 1, counted: 50*m + 1, room: room,
				floor: heapFloor(50*m+1, 1), othersHeld: true,
			},
			want: ErrNoHeapRoom,
		},
		{
			name: "row 3: own and n pass the room, no floor needed",
			w:    heapWeigh{n: 91 * m, own: 10 * m, reserved: 10*m + 1, room: room, othersHeld: true},
			want: ErrNoHeapRoom,
		},
		{
			name: "row 4: fits the floor, another's value is what misses",
			w: heapWeigh{
				n: 40 * m, own: 10 * m, reserved: 60 * m, counted: 60 * m, room: room,
				floor: heapFloor(60*m, 50*m), othersHeld: true,
			},
			want: ErrHeapBusy,
		},
		{
			name: "row 4: fits the floor at margin 0 (SQ-508's site-2 probe as written)",
			w: heapWeigh{
				n: 50*m + 1, reserved: 1, counted: 50 * m, room: room,
				floor: heapFloor(50*m, 1), othersHeld: true,
			},
			want: ErrHeapBusy,
		},
		{
			name: "row 4: own and n fill the room exactly beside another byte",
			w:    heapWeigh{n: 90 * m, own: 10 * m, reserved: 10*m + 1, room: room, othersHeld: true},
			want: ErrHeapBusy,
		},
		{
			name: "sticky busy, first weigh: another's reservation and value miss it",
			w:    heapWeigh{n: 60 * m, reserved: 35 * m, counted: 60 * m, room: room, othersHeld: true},
			want: ErrHeapBusy,
		},
		{
			name: "sticky busy, second weigh: the other was answered after its value was marked",
			w:    heapWeigh{n: 60 * m, reserved: 0, counted: 60 * m, room: room, othersHeld: true},
			want: ErrHeapBusy,
		},
		{
			name: "stale floor: live minus others would miss, but a release followed the collection",
			w:    heapWeigh{n: 60 * m, reserved: 0, counted: 60 * m, room: room, floor: 0, othersHeld: true},
			want: ErrHeapBusy,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := heapVerdict(tc.w)
			if tc.want == nil {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, tc.want)
			}
		})
	}

	assert.Equal(t, int64(0), heapFloor(10, 20), "the floor is never below zero")
}

// verdictRoom gives the heap room bytes past the in-flight bound and
// HeapSlack, with a fake heap (fakeHeap) for one test.
func verdictRoom(t *testing.T, room int64) {
	t.Helper()

	fakeHeap(t, 0, 0)

	bound, _ := InFlight()
	if bound == 0 {
		bound = DefaultInFlight
	}

	debug.SetMemoryLimit(bound + HeapSlack + room)
}

// Test_weighHeap_AStaleFloorIsBusy: a body that would miss the floor were it
// read from a fresh collection is throttled when another reservation is given
// back after the collection began: what it found live can be that request's
// value. Sent again with the floor fresh, the same body is too large.
func Test_weighHeap_AStaleFloorIsBusy(t *testing.T) {
	const room = 100 * verdictMiB

	verdictRoom(t, room)

	// Another request reserves 1 byte and a second one 10 MiB; 55 MiB are
	// live, a table's but for what the two cover.
	small, err := ReserveHeapBeside(1, 0)
	require.NoError(t, err)

	t.Cleanup(small)

	other, err := ReserveHeapBeside(10*verdictMiB, 0)
	require.NoError(t, err)

	live := int64(55 * verdictMiB)
	heap.read = func() (int64, int64) { return live, live }
	heap.collect = func() { other() } // given back after its value was marked

	_, _, err = TakeDenseLane(50*verdictMiB, 50*verdictMiB)
	require.ErrorIs(t, err, ErrHeapBusy, "the floor 55 MiB less 1 byte is not shown: a release followed the collection")

	heap.collect = func() {}

	_, _, err = TakeDenseLane(50*verdictMiB, 50*verdictMiB)
	require.ErrorIs(t, err, ErrNoHeapRoom, "a fresh floor, 55 MiB less 1 byte, leaves no room for 50 MiB")
	assert.Equal(t, int64(1), heap.reserved)
	assert.False(t, denseLane.Load())
}

// Test_TakeDenseLane_StoredDataBesideAOneByteReservationIsTooLarge (S1 at site
// 2): a table of 50 MiB and a byte, which nothing reserves, leaves no room for
// a body of 50 MiB and a byte alone, and none beside another request's 1-byte
// reservation either: what stays without it is the table less that byte. The
// body is too large on every attempt, not throttled.
func Test_TakeDenseLane_StoredDataBesideAOneByteReservationIsTooLarge(t *testing.T) {
	const room = 100 * verdictMiB

	verdictRoom(t, room)

	live := int64(50*verdictMiB + 1)
	heap.read = func() (int64, int64) { return live, live }

	_, _, err := TakeDenseLane(room/2+1, room/2+1)
	require.ErrorIs(t, err, ErrNoHeapRoom, "alone")

	other, err := ReserveHeapBeside(1, 0)
	require.NoError(t, err)

	for attempt := range 3 {
		require.NoError(t, CheckHeapRoom(room/2+1), "attempt %d: site 1 counts nothing live", attempt)

		_, _, err = TakeDenseLane(room/2+1, room/2+1)
		require.ErrorIs(t, err, ErrNoHeapRoom, "attempt %d: beside another request's byte", attempt)
	}

	other()

	assert.Zero(t, heap.reserved)
	assert.False(t, denseLane.Load())
}

// Test_ReserveHeapBeside_StoredDataBesideAOneByteReservationIsTooLarge (S1 at
// the final reservation): with the body's own bytes reserved, a table that
// leaves no room for the estimate alone leaves none beside another request's
// 1-byte reservation either. A body that fits alone beside another's value is
// throttled, and served once that request is answered.
func Test_ReserveHeapBeside_StoredDataBesideAOneByteReservationIsTooLarge(t *testing.T) {
	const room = 100 * verdictMiB

	verdictRoom(t, room)

	own, err := ReserveHeapBeside(10*verdictMiB, 0)
	require.NoError(t, err)

	other, err := ReserveHeapBeside(1, 0)
	require.NoError(t, err)

	live := int64(60 * verdictMiB) // a table, and the body's own bytes
	heap.read = func() (int64, int64) { return live, live }

	_, err = ReserveHeapBeside(40*verdictMiB, 10*verdictMiB)
	require.ErrorIs(t, err, ErrNoHeapRoom, "beside another request's byte")

	other()

	_, err = ReserveHeapBeside(40*verdictMiB, 10*verdictMiB)
	require.ErrorIs(t, err, ErrNoHeapRoom, "alone")

	live = 20 * verdictMiB // the body's bytes and the other request's value

	other, err = ReserveHeapBeside(50*verdictMiB, 0)
	require.NoError(t, err)

	_, err = ReserveHeapBeside(40*verdictMiB, 10*verdictMiB)
	require.ErrorIs(t, err, ErrHeapBusy, "fits once the other is answered")

	other()

	live = 10 * verdictMiB

	release, err := ReserveHeapBeside(40*verdictMiB, 10*verdictMiB)
	require.NoError(t, err, "served once the other is answered")

	release()
	own()

	assert.Zero(t, heap.reserved)
}
