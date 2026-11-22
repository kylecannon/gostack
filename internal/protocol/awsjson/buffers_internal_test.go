package awsjson

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

// The tests here share the package's buffers, so none runs in parallel: Go
// runs every parallel test after the sequential ones.

// isolate empties largeBuffers and pauses its sweeps until t ends, so that a
// collection in the middle of a test drops nothing; the tests call dropIdle
// where they mean a collection to happen.
func isolate(t *testing.T) {
	t.Helper()

	largeBuffers.mu.Lock()
	largeBuffers.paused = true
	largeBuffers.mu.Unlock()

	resetLarge()

	t.Cleanup(func() {
		resetLarge()

		largeBuffers.mu.Lock()
		largeBuffers.paused = false
		largeBuffers.mu.Unlock()
	})
}

// resetLarge empties largeBuffers.
func resetLarge() {
	dropIdle()
	dropIdle()
}

// heldLarge returns what largeBuffers holds: the buffers on its stack, their
// bytes, and the size of big, or zero.
func heldLarge() (count, held, big int) {
	largeBuffers.mu.Lock()
	defer largeBuffers.mu.Unlock()

	if largeBuffers.big != nil {
		big = cap(*largeBuffers.big)
	}

	return len(largeBuffers.bufs), largeBuffers.held, big
}

func bufferOf(size int) *[]byte {
	b := make([]byte, 0, size)

	return &b
}

func TestLargeBufferIsReused(t *testing.T) {
	isolate(t)

	bp := bufferOf(2 * maxSmallBuffer)
	putBuffer(bp)

	if got := getBuffer(); got != bp {
		t.Fatalf("getBuffer returned a buffer of %d bytes, want the large one put back", cap(*got))
	}

	small := bufferOf(1)
	putBuffer(small)

	if n, _, _ := heldLarge(); n != 0 || largeBuffers.count.Load() != 0 {
		t.Fatalf("a small buffer went to largeBuffers: %d held", n)
	}
}

func TestLargeBuffersAreBounded(t *testing.T) {
	isolate(t)

	const size = 1 << 20

	for range 2 * maxLargeHeld / size {
		putBuffer(bufferOf(size))
	}

	putBuffer(bufferOf(maxLargeHeld / 2))
	putBuffer(bufferOf(2 * maxLargeHeld))
	putBuffer(bufferOf(3 * maxLargeHeld / 2))

	n, held, big := heldLarge()
	if held > maxLargeHeld || n != maxLargeHeld/size || big != 2*maxLargeHeld {
		t.Fatalf("largeBuffers holds %d buffers, %d bytes, and one of %d; want %d buffers of %d bytes and one of %d",
			n, held, big, maxLargeHeld/size, size, 2*maxLargeHeld)
	}

	if got := largeBuffers.count.Load(); int(got) != n+1 {
		t.Fatalf("count is %d, want %d", got, n+1)
	}
}

// TestABodyPastWhatLargeBuffersHoldIsReused gives back a buffer bigger than
// all largeBuffers holds and takes it again: one large body called for again
// and again grows its buffer once.
func TestABodyPastWhatLargeBuffersHoldIsReused(t *testing.T) {
	isolate(t)

	bp := bufferOf(2 * maxLargeHeld)
	putBuffer(bp)

	if got := getBuffer(); got != bp {
		t.Fatalf("getBuffer returned a buffer of %d bytes, want the one of %d put back", cap(*got), cap(*bp))
	}

	if n := largeBuffers.count.Load(); n != 0 {
		t.Fatalf("%d buffers still held after the only one was taken", n)
	}
}

func TestLargeBuffersGoWhenIdle(t *testing.T) {
	isolate(t)

	putBuffer(bufferOf(2 * maxSmallBuffer))
	putBuffer(bufferOf(2 * maxLargeHeld))
	dropIdle()

	if n, _, big := heldLarge(); n != 1 || big == 0 {
		t.Fatal("a buffer given back since the last collection was dropped")
	}

	dropIdle()

	if n, held, big := heldLarge(); n != 0 || held != 0 || big != 0 || largeBuffers.count.Load() != 0 {
		t.Fatalf("an idle buffer was kept: %d held, %d bytes, big %d", n, held, big)
	}
}

// TestAPausedSweepDropsNothing runs the sweep a collection runs, twice, in the
// middle of a test: a test's buffers are dropped only by the dropIdle it calls.
func TestAPausedSweepDropsNothing(t *testing.T) {
	isolate(t)

	putBuffer(bufferOf(2 * maxSmallBuffer))
	sweep()
	sweep()

	if n, _, _ := heldLarge(); n != 1 {
		t.Fatalf("a paused sweep dropped a buffer: %d held, want 1", n)
	}
}

// discard is a ResponseWriter that keeps nothing.
type discard struct{}

func (discard) Header() http.Header         { return http.Header{} }
func (discard) WriteHeader(int)             {}
func (discard) Write(p []byte) (int, error) { return len(p), nil }

// filler is an Appender that writes a json string of n bytes, a KiB at a
// time, so that its buffer grows by append as an encoder's does.
type filler int

var fillerKiB = []byte(strings.Repeat("a", 1<<10))

func (n filler) AppendJSON(b []byte) ([]byte, error) {
	b = append(b, '"')

	for left := int(n); left > 0; left -= len(fillerKiB) {
		b = append(b, fillerKiB[:min(left, len(fillerKiB))]...)
	}

	return append(b, '"'), nil
}

// TestWriteResponseReusesALargeBody writes a 20 MiB body, past what a buffer
// once grew to, again and again: after the first, the calls allocate a small
// part of its size, where growing it from nothing each time allocated five
// times its size per call.
func TestWriteResponseReusesALargeBody(t *testing.T) {
	isolate(t)

	const size, calls = 20 << 20, 4

	body := filler(size)
	WriteResponse(httptest.NewRecorder(), "r", "1.0", body)

	var before, after runtime.MemStats

	runtime.ReadMemStats(&before)

	for range calls {
		WriteResponse(discard{}, "r", "1.0", body)
	}

	runtime.ReadMemStats(&after)

	if got := after.TotalAlloc - before.TotalAlloc; got > size/2 {
		t.Fatalf("%d calls of a %d-byte body allocated %d bytes", calls, size, got)
	}
}
