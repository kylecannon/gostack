package protocol

import (
	"math"
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Test_OverMemory_WeighsACostAgainstTheGoMemoryLimit: with a Go memory limit
// set, a decoding whose cost keeps more of the heap than the limit allows is
// over it, at costPerHeapByte of cost for a byte of heap; with no limit
// nothing is.
func Test_OverMemory_WeighsACostAgainstTheGoMemoryLimit(t *testing.T) {
	const cost = 400 << 20 // about 50 MiB of heap

	old := debug.SetMemoryLimit(-1)

	t.Cleanup(func() { debug.SetMemoryLimit(old) })

	for _, tt := range []struct {
		name  string
		limit int64
		over  bool
	}{
		{"no limit", math.MaxInt64, false},
		{"a limit that holds it", 64 << 20, false},
		{"a limit that is exactly it", cost / costPerHeapByte, false},
		{"a limit that does not", 40 << 20, true},
	} {
		debug.SetMemoryLimit(tt.limit)

		assert.Equal(t, tt.over, OverMemory(cost), tt.name)
	}
}
