package value

import (
	"math"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
)

// spliceSink keeps each spliced item on the heap.
var spliceSink Packed

// Test_spliceJoin_allocatesOnce: a splice that adds an attribute to a stored
// item writes the result in one allocation the size of the item, on each side
// of the sizes where the header's size field grows a byte, 127/128 and
// 16383/16384, and at 64 KiB, where the runtime rounds the allocation up to
// whole pages. It is not parallel: AllocsPerRun refuses a parallel test.
func Test_spliceJoin_allocatesOnce(t *testing.T) {
	key := Item{"pk": {S: new("k")}}

	stored, err := Pack(key)
	require.NoError(t, err)

	for _, size := range []int{127, 128, 16383, 16384, 64 << 10} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			// The key holds three bytes and the name n one.
			values := Item{"n": {S: new(strings.Repeat("x", size-len("pk")-len("k")-len("n")))}}
			want, err := Splice(stored, []string{"n"}, values)
			require.NoError(t, err)
			require.Equal(t, size, want.Size())

			requireJoinAllocatesOnce(t, stored, want, values)
		})
	}
}

// Test_spliceJoin_replaceAllocatesOnce: a splice that writes over an
// attribute of a small item, as UpdateItem's SET of one number does, writes
// the result in one allocation of the item's size, though the member it
// removes is more of the item than Pack's slack allows: the result's length is
// counted before it is written. The payloads run through every length from 0
// to 63 bytes, so the result's length crosses the runtime's size classes, 16
// bytes apart there, and a length counted one short would need a second
// allocation at the first past a class.
func Test_spliceJoin_replaceAllocatesOnce(t *testing.T) {
	const swept = 64

	payloads := make([]int, swept, swept+2)
	for payload := range swept {
		payloads[payload] = payload
	}

	payloads = append(payloads, 100, 4<<10)

	for _, payload := range payloads {
		t.Run(strconv.Itoa(payload), func(t *testing.T) {
			stored, err := Pack(Item{
				"id": {S: new("k")},
				"n":  {N: new("12345678901234567890")},
				"d":  {S: new(strings.Repeat("x", payload))},
			})
			require.NoError(t, err)

			values := Item{"n": {N: new("2")}}
			want, err := Splice(stored, []string{"n"}, values)
			require.NoError(t, err)

			requireJoinAllocatesOnce(t, stored, want, values)
		})
	}
}

// requireJoinAllocatesOnce holds that the runs and join of a splice of stored
// that wrote values, of which want is the result, allocate want alone.
func requireJoinAllocatesOnce(t *testing.T, stored, want Packed, values Item) {
	t.Helper()

	wh := want.header()
	h := packHeader{size: wh.size, scalars: wh.scalars, attrs: wh.attrs}
	body := string(stored[stored.header().body:])

	added, err := Pack(values)
	require.NoError(t, err)

	members := added.JSON()
	members = members[1 : len(members)-1]

	require.Equal(t, want, spliceRunsJoin(h, body, members))

	allocs := testing.AllocsPerRun(100, func() {
		spliceSink = spliceRunsJoin(h, body, members)
	})
	require.InDelta(t, 1.0, allocs, 0, "allocations")

	// The least of a few tries, since another goroutine can allocate in
	// between.
	var before, after runtime.MemStats

	least := uint64(math.MaxUint64)

	for range 5 {
		runtime.ReadMemStats(&before)
		spliceSink = spliceRunsJoin(h, body, members)
		runtime.ReadMemStats(&after)
		least = min(least, after.TotalAlloc-before.TotalAlloc)
	}

	require.LessOrEqual(t, least, uint64(allocBytes(len(want))), "bytes allocated for %d", len(want))
}

// spliceRunsJoin is the part of SpliceOver that writes the result, for a
// splice of the one attribute n: the runs it keeps of body and their join
// with the members added.
func spliceRunsJoin(h packHeader, body, added string) Packed {
	var inline [spliceRunsInline]span

	runs, _ := spliceRuns(inline[:0], body, []string{"n"})

	return spliceJoin(h, body, runs, added)
}

// Test_unpacker_flagsShareAWord: the flag the splice added to unpacker,
// sorted, sits in the padding after open and chunked, so an unpacker is no
// larger than on main. A word more grew the frame of Orderer.object, which
// the snapshot runs per item, and added instructions to its member loop.
func Test_unpacker_flagsShareAWord(t *testing.T) {
	t.Parallel()

	var u unpacker

	require.Equal(t, unsafe.Offsetof(u.open)+unsafe.Sizeof(uintptr(0)), unsafe.Offsetof(u.parts))
	require.Equal(t, unsafe.Offsetof(u.open)+2, unsafe.Offsetof(u.sorted))
}
