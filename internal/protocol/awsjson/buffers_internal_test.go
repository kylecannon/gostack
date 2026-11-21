package awsjson

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"testing"
)

// The tests here share the package's buffers, so none runs in parallel: Go
// runs every parallel test after the sequential ones.

// isolate empties largeBuffers and stops collections until t ends, so that
// no sweep runs in the middle of a test.
func isolate(t *testing.T) {
	t.Helper()

	resetLarge()

	gcPercent := debug.SetGCPercent(-1)

	t.Cleanup(func() {
		debug.SetGCPercent(gcPercent)
		resetLarge()
	})
}

// resetLarge empties largeBuffers.
func resetLarge() {
	dropIdle()
	dropIdle()
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

	if n := largeBuffers.count.Load(); n != 0 {
		t.Fatalf("a small buffer went to largeBuffers: %d held", n)
	}
}

func TestLargeBuffersAreBounded(t *testing.T) {
	isolate(t)

	const size = 1 << 20

	for range 2 * maxLargeHeld / size {
		putBuffer(bufferOf(size))
	}

	putBuffer(bufferOf(2 * maxLargeHeld))

	if largeBuffers.held > maxLargeHeld || int(largeBuffers.count.Load()) != maxLargeHeld/size {
		t.Fatalf("largeBuffers holds %d buffers, %d bytes; want %d buffers of %d bytes",
			largeBuffers.count.Load(), largeBuffers.held, maxLargeHeld/size, size)
	}
}

func TestLargeBuffersGoWhenIdle(t *testing.T) {
	isolate(t)

	putBuffer(bufferOf(2 * maxSmallBuffer))
	dropIdle()

	if largeBuffers.count.Load() != 1 {
		t.Fatal("a buffer given back since the last collection was dropped")
	}

	dropIdle()

	if largeBuffers.count.Load() != 0 || largeBuffers.held != 0 {
		t.Fatalf("an idle buffer was kept: %d held, %d bytes", largeBuffers.count.Load(), largeBuffers.held)
	}
}

// discard is a ResponseWriter that keeps nothing.
type discard struct{}

func (discard) Header() http.Header         { return http.Header{} }
func (discard) WriteHeader(int)             {}
func (discard) Write(p []byte) (int, error) { return len(p), nil }

// filler is an Appender that writes a json string of n bytes.
type filler int

func (n filler) AppendJSON(b []byte) ([]byte, error) {
	b = append(b, '"')

	for range int(n) {
		b = append(b, 'a')
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
