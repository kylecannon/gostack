// Package heaptest settles the heap for tests whose answer the decode
// budget's heap room decides (protocol.ReserveHeap, protocol.HeapCouldFit).
//
// A test at the room's edge must weigh the heap it builds, not one an earlier
// test left behind. Settle handles one way an earlier test's state outlives
// it: a sync.Pool keeps what was put in it through the first collection after
// the Put, in its victim cache, and frees it at the second. A gateway's router
// pools its routing contexts, each naming the router, so a dropped gateway
// and the table it served outlive the next collection, and a decision whose
// own collection is that first one counts the dead table as live. Settle runs
// two collections first.
//
// Collections do not free what something still reaches. A live timer keeps
// what its function names, and so does a stopped one until the runtime clears
// it from its timer heap. Tests close what they start and cut such links
// themselves; Settle does not cover them. Its log, and the log it adds when the
// test fails, show what was live when the test decided.
package heaptest

import (
	"runtime"
	"runtime/metrics"
	"testing"
)

// The runtime metrics Settle reports: the heap's objects, which
// protocol.ReserveHeap weighs, and the collections that have run.
const (
	heapObjects = "/memory/classes/heap/objects:bytes"
	collections = "/gc/cycles/total:gc-cycles"
)

// Settle runs two full collections, the first moving every sync.Pool's
// contents to its victim cache and the second freeing them, and logs the
// heap's objects and the collection count after them. If tb fails, the two
// are logged again at its end. What is still reachable after the two stays
// live; the log shows how much.
func Settle(tb testing.TB) {
	tb.Helper()

	runtime.GC() //nolint:revive // a pool keeps what it holds through the first collection
	runtime.GC() //nolint:revive // and frees it at the second

	objects, cycles := read()
	tb.Logf("heap settled: objects %.1f MiB, NumGC %d", mib(objects), cycles)

	tb.Cleanup(func() {
		if tb.Failed() {
			objects, cycles := read()
			tb.Logf("heap at failure: objects %.1f MiB, NumGC %d", mib(objects), cycles)
		}
	})
}

// mebibyte is the unit Settle logs the heap in.
const mebibyte = 1 << 20

// mib is n bytes in MiB.
func mib(n uint64) float64 {
	return float64(n) / mebibyte
}

// read reads the heap's objects and the collections run so far.
func read() (objects, cycles uint64) {
	samples := []metrics.Sample{{Name: heapObjects}, {Name: collections}}
	metrics.Read(samples)

	return samples[0].Value.Uint64(), samples[1].Value.Uint64()
}
