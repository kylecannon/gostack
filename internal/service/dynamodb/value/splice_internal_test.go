package value

import (
	"math"
	"runtime"
	"strconv"
	"strings"
	"testing"

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
// counted before it is written.
func Test_spliceJoin_replaceAllocatesOnce(t *testing.T) {
	for _, payload := range []int{0, 100, 4 << 10} {
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
