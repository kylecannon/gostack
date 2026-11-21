package protocol

import (
	"math"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"sync"
)

// HeapSlack is what ReserveHeap leaves under the Go memory limit beyond the
// in-flight bound: the runtime's own structures and the stacks, the spans the
// heap's objects leave part empty, and what a handler allocates with a decoded
// value (a stored item, a response). docs/architecture.md, "Memory under a
// container limit", has the measurements it is set from.
const HeapSlack = 48 << 20

// heap is what ReserveHeap has reserved, and the samples it reads the heap
// with. Reading and reserving are one step under mu, so that requests decoded
// at once each see what the others reserved. collecting lets one request at a
// time run a collection for room.
var heap = struct {
	mu         sync.Mutex
	reserved   int64
	collecting sync.Mutex
	read       func() (objects, live int64) // readHeap; a test replaces it
	collect    func()                       // runtime.GC; a test replaces it
}{
	read:    readHeap,
	collect: runtime.GC,
}

const (
	// heapObjects is every object on the heap now, live or not yet swept:
	// never less than what is live.
	heapObjects = "/memory/classes/heap/objects:bytes"
	// heapLive is what the last collection found live: what a collection now
	// would leave, unless more was kept since.
	heapLive = "/gc/heap/live:bytes"
)

// ReserveHeap reserves estimate bytes of heap for a decoding that keeps at most
// that much at its peak, and returns the release that gives them back once it
// is decoded. It reports false, reserving nothing, when there is no Go memory
// limit (debug.SetMemoryLimit, which cmd/gostack sets from a container's), or
// when the heap's objects now, what is reserved already and estimate pass the
// limit less the in-flight bound, which what every other request's body and
// decoding holds stays below, and HeapSlack. Objects not yet collected count
// as live; when only they stand in the way, it runs one collection, one
// request at a time, and reads the heap again. A decoder refuses the body when
// it reports false: decoding it could end in the container's OOM killer, which
// waiting does not prevent.
func ReserveHeap(estimate int64) (release func(), ok bool) {
	limit := debug.SetMemoryLimit(-1)
	if limit == math.MaxInt64 {
		return nil, false
	}

	bound, _ := InFlight()
	if bound == 0 {
		bound = DefaultInFlight
	}

	room := limit - bound - HeapSlack

	fits, collectable := tryReserve(estimate, room)
	if !fits && collectable && heap.collecting.TryLock() {
		heap.collect()
		heap.collecting.Unlock()

		fits, _ = tryReserve(estimate, room)
	}

	if !fits {
		return nil, false
	}

	return func() {
		heap.mu.Lock()
		heap.reserved -= estimate
		heap.mu.Unlock()
	}, true
}

// tryReserve reserves estimate when the heap's objects, what is reserved and
// estimate fit room, and otherwise reports whether they would fit were only what
// the last collection found live left.
func tryReserve(estimate, room int64) (fits, collectable bool) {
	heap.mu.Lock()
	defer heap.mu.Unlock()

	objects, live := heap.read()
	if objects+heap.reserved+estimate <= room {
		heap.reserved += estimate

		return true, false
	}

	return false, live+heap.reserved+estimate <= room
}

// readHeap reads the heap's objects and what the last collection found live,
// with heap.mu held, which guards heapSamples.
func readHeap() (objects, live int64) {
	metrics.Read(heapSamples)

	objects = int64(heapSamples[0].Value.Uint64()) //nolint:gosec // the heap fits an int64
	live = int64(heapSamples[1].Value.Uint64())    //nolint:gosec // the heap fits an int64

	return objects, live
}

var heapSamples = []metrics.Sample{{Name: heapObjects}, {Name: heapLive}}
