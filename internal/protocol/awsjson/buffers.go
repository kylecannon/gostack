package awsjson

import (
	"runtime"
	"sync"
	"sync/atomic"
)

// The bodies the generated encoders write go into reused buffers. A buffer of
// up to maxSmallBuffer goes back to smallBuffers, a sync.Pool. A larger one
// goes to largeBuffers, which holds at most maxLargeHeld bytes of them and
// drops them all after a collection when none was taken or given back since
// the one before. A sync.Pool would keep every large buffer a burst grew for
// two collections, a GiB after 64 callers of 16 MiB, where this keeps
// maxLargeHeld; and with no reuse at all, each large body would grow from
// nothing on every call, about five times its size in allocation.
const (
	maxSmallBuffer = 256 << 10
	maxLargeHeld   = 32 << 20
)

var smallBuffers = sync.Pool{New: func() any { return new([]byte) }}

// largeBuffers is a stack of large buffers. count mirrors len(bufs), so that
// getBuffer finds an empty stack without the lock. used records a take since
// the last collection.
var largeBuffers struct {
	mu    sync.Mutex
	bufs  []*[]byte
	held  int
	count atomic.Int32
	used  bool
	sweep sync.Once
}

// getBuffer returns an empty buffer: a large one when one is held, since the
// size of the body is not known yet, else one from smallBuffers.
func getBuffer() *[]byte {
	if largeBuffers.count.Load() > 0 {
		largeBuffers.mu.Lock()

		if n := len(largeBuffers.bufs); n > 0 {
			bp := largeBuffers.bufs[n-1]
			largeBuffers.bufs[n-1] = nil
			largeBuffers.bufs = largeBuffers.bufs[:n-1]
			largeBuffers.held -= cap(*bp)
			largeBuffers.count.Store(int32(n - 1)) //nolint:gosec // n is at most maxLargeHeld/maxSmallBuffer
			largeBuffers.used = true
			largeBuffers.mu.Unlock()

			return bp
		}

		largeBuffers.mu.Unlock()
	}

	bp, _ := smallBuffers.Get().(*[]byte)

	return bp
}

// putBuffer gives bp back for reuse, emptied. A large buffer that does not fit
// in what largeBuffers may hold is dropped.
func putBuffer(bp *[]byte) {
	*bp = (*bp)[:0]

	size := cap(*bp)
	if size <= maxSmallBuffer {
		smallBuffers.Put(bp)

		return
	}

	largeBuffers.sweep.Do(armSweep)
	largeBuffers.mu.Lock()

	if largeBuffers.held+size <= maxLargeHeld {
		largeBuffers.bufs = append(largeBuffers.bufs, bp)
		largeBuffers.held += size
		largeBuffers.count.Store(int32(len(largeBuffers.bufs))) //nolint:gosec // at most maxLargeHeld/maxSmallBuffer
	}

	largeBuffers.used = true

	largeBuffers.mu.Unlock()
}

// gcMark is an object whose collection marks the end of a GC cycle. It holds a
// pointer so that it is not packed with other small objects, whose cleanups
// can then wait on all of them.
type gcMark struct{ _ *byte }

// armSweep runs sweepLarge after the next collection.
func armSweep() {
	runtime.AddCleanup(&gcMark{}, sweepLarge, struct{}{})
}

// sweepLarge runs dropIdle, then waits for the next collection.
func sweepLarge(struct{}) {
	dropIdle()
	armSweep()
}

// dropIdle drops the large buffers when none was taken or given back since it
// last ran.
func dropIdle() {
	largeBuffers.mu.Lock()

	if !largeBuffers.used {
		clear(largeBuffers.bufs)
		largeBuffers.bufs = largeBuffers.bufs[:0]
		largeBuffers.held = 0
		largeBuffers.count.Store(0)
	}

	largeBuffers.used = false
	largeBuffers.mu.Unlock()
}
