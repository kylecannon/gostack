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

			wh := want.header()
			h := packHeader{size: wh.size, scalars: wh.scalars, attrs: wh.attrs}
			body := string(stored[stored.header().body:])

			added, err := Pack(values)
			require.NoError(t, err)

			members := added.JSON()
			members = members[1 : len(members)-1]

			require.Equal(t, want, spliceJoin(h, body, []string{"n"}, members))

			allocs := testing.AllocsPerRun(100, func() {
				spliceSink = spliceJoin(h, body, []string{"n"}, members)
			})
			require.InDelta(t, 1.0, allocs, 0, "allocations")

			// The least of a few tries, since another goroutine can allocate
			// in between.
			var before, after runtime.MemStats

			least := uint64(math.MaxUint64)

			for range 5 {
				runtime.ReadMemStats(&before)
				spliceSink = spliceJoin(h, body, []string{"n"}, members)
				runtime.ReadMemStats(&after)
				least = min(least, after.TotalAlloc-before.TotalAlloc)
			}

			require.LessOrEqual(t, least, uint64(allocBytes(len(want))), "bytes allocated for %d", len(want))
		})
	}
}
