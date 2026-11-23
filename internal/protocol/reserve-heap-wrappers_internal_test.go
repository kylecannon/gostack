package protocol

import "context"

// The bool forms of the heap's weighs that the package's tests read with. No
// production code calls them: a decoder needs heapVerdict's two refusals apart,
// ErrHeapBusy a throttle and ErrNoHeapRoom too large, which a bool hides.

// ReserveHeap is ReserveHeapBeside(estimate, 0), reporting whether it
// reserved.
func ReserveHeap(estimate int64) (release func(), ok bool) {
	release, err := ReserveHeapBeside(estimate, 0)

	return release, err == nil
}

// HeapCouldFit reports whether n fits beside what is reserved and what the last
// collection found live, collecting first when only that stands in the way, as
// TakeDenseLane weighs its projected heap. It reserves nothing.
func HeapCouldFit(n int64) bool {
	return weighHeap(context.Background(), n, 0, checkLive) == nil
}

// HeapRoomFor is CheckHeapRoom(n) == nil.
func HeapRoomFor(n int64) bool {
	return CheckHeapRoom(n) == nil
}
