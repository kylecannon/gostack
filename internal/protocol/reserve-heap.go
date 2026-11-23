package protocol

import (
	"errors"
	"math"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"sync"
	"sync/atomic"
)

// HeapSlack is what ReserveHeap leaves under the Go memory limit beyond the
// in-flight bound: the runtime's own structures and the stacks, the spans the
// heap's objects leave part empty, and what a handler allocates with a decoded
// value (a stored item, a response). docs/architecture.md, "Memory under a
// container limit", has the measurements it is set from.
const HeapSlack = 48 << 20

// HeapKeeper is a DecodeHolder that keeps a decoding's heap reservations
// (ReserveHeap) until its request is answered, so that what the handler builds
// from the decoded value stays counted while it works.
type HeapKeeper interface {
	// KeepHeap takes a reservation's release, to run when the request is
	// answered, and reports whether it took it. When it did not, the decoder
	// runs release once the body is decoded.
	KeepHeap(release func()) bool
	// ReleasePool gives back what the request's body and its decoding hold of
	// the in-flight bound, for a body whose bytes a heap reservation counts
	// from then on, so that a body decoded past DecodeBudget holds the bound
	// no longer than one refused there.
	ReleasePool()
	// PoolHeld is what ReleasePool would give back.
	PoolHeld() int64
}

// heap is what ReserveHeap has reserved, and the samples it reads the heap
// with. Reading and reserving are one step under mu, so that requests decoded
// at once each see what the others reserved. Only the body holding the dense
// lane (TakeDenseLane) runs a collection for room, so one runs at a time.
var heap = struct {
	mu       sync.Mutex
	reserved int64
	read     func() (objects, live int64) // readHeap; a test replaces it
	collect  func()                       // runtime.GC; a test replaces it
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
// is decoded. It reports false, reserving nothing, when the heap's objects
// now, what is reserved already and estimate pass the room heapRoom weighs
// them against: the Go memory limit (debug.SetMemoryLimit, which cmd/gostack
// sets from a container's or a machine's memory), no more than the memory cap,
// less the in-flight bound, which what every other request's body and
// decoding holds stays below, and HeapSlack. Objects not yet collected count
// as live; when the heap stands in the way, it runs one collection and reads
// the heap again, unless what is reserved and estimate pass the room
// alone. It is ReserveHeapBeside(estimate, 0).
func ReserveHeap(estimate int64) (release func(), ok bool) {
	release, err := ReserveHeapBeside(estimate, 0)

	return release, err == nil
}

// ReserveHeapBeside is ReserveHeap for a request that holds own bytes of what
// is reserved already: the rest is other requests'. When estimate does not fit
// it fails with ErrHeapBusy when other requests reserve some of the room and
// own and estimate fit it alone, since the reservations, and the values their
// handlers hold, are given back once those requests are answered, and with
// ErrNoHeapRoom otherwise. A decoder throttles a body on ErrHeapBusy and
// refuses it on ErrNoHeapRoom: decoding it now could end in the container's
// OOM killer, and one that misses with nothing reserved by others, or that
// passes the room alone, would miss again.
func ReserveHeapBeside(estimate, own int64) (release func(), err error) {
	err = weighHeap(estimate, own, reserveObjects)
	if err != nil {
		return nil, err
	}

	return func() {
		heap.mu.Lock()
		heap.reserved -= estimate
		heap.mu.Unlock()
	}, nil
}

// heapUse is what weighHeap does with n.
type heapUse int

const (
	// checkLive weighs n beside what the last collection found live and
	// reserves nothing (HeapCouldFit).
	checkLive heapUse = iota
	// reserveObjects weighs n beside the heap's objects and reserves it when
	// it fits (ReserveHeap).
	reserveObjects
)

// weighHeap weighs n against the room (heapRoom) beside what is reserved, own
// bytes of it the caller's, and the heap, as use says. When the heap stands in
// the way it runs one collection and weighs again, unless what is reserved and
// n pass the room alone. It fails as ReserveHeapBeside does.
func weighHeap(n, own int64, use heapUse) error {
	room := heapRoom()

	collectable, err := tryHeap(n, own, room, use)
	if err != nil && collectable {
		heap.collect()

		_, err = tryHeap(n, own, room, use)
	}

	return err
}

// heapRoom is what ReserveHeap weighs the heap against: the Go memory limit,
// no more than the memory cap (SetMemoryCap), less the in-flight bound and
// HeapSlack. With neither a limit nor a cap it takes SoftLimit(UnknownMemory)
// as the limit.
func heapRoom() int64 {
	limit := debug.SetMemoryLimit(-1)
	if memoryCap := memoryCap.Load(); memoryCap > 0 {
		limit = min(limit, memoryCap)
	}

	if limit == math.MaxInt64 {
		limit = SoftLimit(UnknownMemory)
	}

	bound, _ := InFlight()
	if bound == 0 {
		bound = DefaultInFlight
	}

	return limit - bound - HeapSlack
}

const (
	// MemoryPercent is how much of the memory the process may use, its
	// container's limit or else its machine's, becomes its Go memory limit
	// (cmd/gostack) and the memory cap (SetMemoryCap). The runtime counts what
	// it has mapped for heap, stacks and its own tables; the container also
	// counts the binary, the page cache and the services' tmpfs, so the rest
	// is left to them. docs/architecture.md, "Memory under a container limit",
	// has the measurements the number comes from.
	MemoryPercent = 85

	// UnknownMemory is the memory heapRoom takes as the process's when nothing
	// says what it may use, no Go memory limit and no memory cap: on darwin,
	// where cmd/gostack reads no memory, and in a test.
	UnknownMemory = 1 << 30

	percent = 100
)

// SoftLimit is MemoryPercent of memory bytes.
func SoftLimit(memory int64) int64 {
	return memory / percent * MemoryPercent
}

// memoryCap is the most heapRoom takes as the Go memory limit (SetMemoryCap);
// zero is none.
var memoryCap atomic.Int64

// SetMemoryCap makes n the most ReserveHeap takes as the Go memory limit,
// whatever GOMEMLIMIT set the runtime's to, and returns the cap it replaces.
// cmd/gostack sets it to SoftLimit of the memory its container, or else its
// machine, has. Zero or less removes it.
func SetMemoryCap(n int64) (old int64) {
	return memoryCap.Swap(max(n, 0))
}

// HeapCouldFit reports whether ReserveHeap could reserve n now: whether what
// is live, what is reserved and n fit the room ReserveHeap weighs them against
// (heapRoom). It reads what is live from the last collection; when that leaves
// no room, it runs a collection, as ReserveHeap does, and reads again, because
// a collection that ran while an earlier body's value was still live would
// otherwise refuse the next body for garbage. When what is reserved and n pass
// the room alone it reports false without collecting (HeapRoomFor). It
// reserves nothing, so a decoder can refuse a body early that ReserveHeap
// would refuse at the end of its walk.
func HeapCouldFit(n int64) bool {
	return weighHeap(n, 0, checkLive) == nil
}

// HeapRoomFor reports whether n fits the room ReserveHeap weighs the heap
// against beside what is reserved, were nothing else live: a body it refuses,
// HeapCouldFit and ReserveHeap refuse too, whatever a collection would free.
// It is CheckHeapRoom(n) == nil.
func HeapRoomFor(n int64) bool {
	return CheckHeapRoom(n) == nil
}

// CheckHeapRoom is HeapRoomFor for a decoder: it fails with ErrHeapBusy when n
// passes the room beside what is reserved but fits it alone, so that what
// other requests reserve misses it only until they are answered, and with
// ErrNoHeapRoom when n passes the room alone. It reads what is reserved and
// the room, never the heap, and runs no collection.
func CheckHeapRoom(n int64) error {
	room := heapRoom()

	heap.mu.Lock()
	defer heap.mu.Unlock()

	switch {
	case heap.reserved+n <= room:
		return nil
	case n <= room:
		return ErrHeapBusy
	default:
		return ErrNoHeapRoom
	}
}

// denseLane is held by the one body at a time that is walked and decoded past
// DecodeBudget (TakeDenseLane).
var denseLane atomic.Bool

var (
	// ErrDenseLaneTaken is TakeDenseLane's refusal while another body holds
	// the lane: the body could be decoded once the lane is free.
	ErrDenseLaneTaken = errors.New("another body holds the dense lane")
	// ErrHeapBusy is the refusal of a body the heap has no room for while
	// other requests hold reservations, when it would fit with none of theirs:
	// it fits once they are answered.
	ErrHeapBusy = errors.New("other requests' reservations hold the heap's room")
	// ErrNoHeapRoom is the refusal of a body the heap has no room for with
	// nothing other requests reserve.
	ErrNoHeapRoom = errors.New("no room on the heap")
)

// TakeDenseLane is ReserveHeap(n) for a body whose decoding passes
// DecodeBudget, the only kind of body decoded off the in-flight bound, and
// whose decoding is projected to keep peak, its own n bytes included: such a
// body first takes the process's one dense lane, so at most one of them is
// walked and decoded at a time and none waits for another. It fails, holding
// nothing, with ErrDenseLaneTaken when another body has the lane, and, as
// ReserveHeapBeside does, with ErrHeapBusy or ErrNoHeapRoom when the heap
// could not hold peak (HeapCouldFit, which may collect, one lane holder at a
// time) or has no room for n. Otherwise lane gives back the lane, which the
// decoder does as soon as decoding returns, and runs once however often it is
// called; release gives back the reservation, which lasts as ReserveHeap's
// does, until the request is answered, so the lane bounds the decodes at once
// and the reservation the heap.
func TakeDenseLane(n, peak int64) (lane, release func(), err error) {
	if !denseLane.CompareAndSwap(false, true) {
		return nil, nil, ErrDenseLaneTaken
	}

	err = weighHeap(peak, 0, checkLive)
	if err == nil {
		release, err = ReserveHeapBeside(n, 0)
	}

	if err != nil {
		denseLane.Store(false)

		return nil, nil, err
	}

	var once sync.Once

	return func() { once.Do(func() { denseLane.Store(false) }) }, release, nil
}

// tryHeap weighs n beside what is reserved, own bytes of it the caller's, and
// the heap: its objects for reserveObjects, which then reserves n when they
// fit room, and what the last collection found live for checkLive. On a miss
// it reports whether what is reserved and n would fit were nothing live,
// whether a collection could make room, and fails as ReserveHeapBeside does.
// What the last collection found live does not say what a reservation can
// take: it can have run while an earlier body's value was live.
func tryHeap(n, own, room int64, use heapUse) (collectable bool, err error) {
	heap.mu.Lock()
	defer heap.mu.Unlock()

	objects, live := heap.read()
	if use == checkLive {
		objects = live
	}

	switch {
	case objects+heap.reserved+n <= room:
		if use == reserveObjects {
			heap.reserved += n
		}

		return false, nil
	case heap.reserved > own && own+n <= room:
		return heap.reserved+n <= room, ErrHeapBusy
	default:
		return heap.reserved+n <= room, ErrNoHeapRoom
	}
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
