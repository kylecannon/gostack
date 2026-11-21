package protocol

import (
	"math"
	"runtime/debug"
)

const (
	// costPerHeapByte is how many bytes of decode cost one byte of the heap
	// stands for at its peak. The cost model charges two passes and append's
	// regrowth: a list of {"N":"1"} is charged 1,638 bytes an element and keeps
	// about 216 live, 7.6 to 1, as many short lists do. One long list also
	// holds the slice it grew from at the peak, so a decoder that charges its
	// largest slice twice (awsjson) is at 4.4 to 1 for it.
	costPerHeapByte = 8

	// heapShare is the part of the memory limit, as a divisor, that what one
	// decoding keeps live may take: all of it. The Go memory limit is already
	// a share of the container's, which leaves the rest to the runtime.
	heapShare = 1
)

// OverMemory reports whether a decoding that costs cost keeps more of the heap
// at its peak than the Go memory limit as it is now (debug.SetMemoryLimit,
// which cmd/gostack sets from a container's) allows. A decoder that charges
// what it holds with a DecodeHolder passes cost and what its largest slice
// holds, and refuses a body for which this is true: waiting does not make room
// for it, and decoding it ends in the container's OOM killer. With no limit
// nothing is over it.
func OverMemory(cost int64) bool {
	limit := debug.SetMemoryLimit(-1)

	return limit < math.MaxInt64 && cost/costPerHeapByte > limit/heapShare
}
