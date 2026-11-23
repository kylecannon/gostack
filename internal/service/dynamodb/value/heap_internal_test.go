package value

import (
	"math"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// allocSink keeps each measured allocation on the heap.
var allocSink []byte

// Test_allocBytes_coversSizeClass: for every request up to 2 KiB and every
// seventh one past it to 80 KiB, an allocation takes at most what allocBytes
// says, as the runtime's own count of allocated bytes shows.
func Test_allocBytes_coversSizeClass(t *testing.T) {
	var stats runtime.MemStats

	const (
		dense = 2 << 10
		top   = 80 << 10
		step  = 7
	)

	// took is what one allocation of n bytes added to the count, the least
	// of three tries, since another goroutine can allocate in between.
	took := func(n int) int {
		least := math.MaxInt

		for range 3 {
			runtime.ReadMemStats(&stats)
			before := stats.TotalAlloc

			allocSink = make([]byte, n)

			runtime.ReadMemStats(&stats)
			least = min(least, int(stats.TotalAlloc-before))
		}

		return least
	}

	for n := 1; n <= top; {
		require.GreaterOrEqual(t, allocBytes(n), took(n), "allocating %d bytes", n)

		if n < dense {
			n++
		} else {
			n += step
		}
	}

	allocSink = nil
}
