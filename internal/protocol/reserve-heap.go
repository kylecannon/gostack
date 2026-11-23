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

// HeapSlack is what a heap reservation (ReserveHeapBeside) leaves under the Go
// memory limit beyond the in-flight bound: the runtime's own structures and
// the stacks, the spans the heap's objects leave part empty, and what a
// handler allocates with a decoded value (a stored item, a response).
// docs/architecture.md, "Memory under a container limit", has the
// measurements it is set from.
const HeapSlack = 48 << 20

// HeapKeeper is a DecodeHolder that keeps a decoding's heap reservations
// (ReserveHeapBeside) until its request is answered, so that what the handler
// builds from the decoded value stays counted while it works.
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

// heap is what ReserveHeapBeside has reserved, how many reservations have
// been given back, and the samples it reads the heap with. Reading and
// reserving are one step under mu, so that requests decoded at once each see
// what the others reserved. Only the body holding the dense lane
// (TakeDenseLane) runs a collection for room, so one runs at a time.
var heap = struct {
	mu       sync.Mutex
	reserved int64
	// releases counts the reservations given back. A count unchanged since a
	// collection began says that what it found live is not overstated by a
	// value whose reservation was given back after it was marked (heapFloor).
	releases uint64
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

// ReserveHeapBeside reserves estimate bytes of heap for a decoding that keeps
// at most that much at its peak, for a request that holds own bytes of what is
// reserved already (the rest is other requests'), and returns the release that
// gives them back. It reserves when the heap's objects now, what is reserved
// already and estimate fit the room heapRoom weighs them against: the Go
// memory limit (debug.SetMemoryLimit, which cmd/gostack sets from a
// container's or a machine's memory), no more than the memory cap, less the
// in-flight bound, which what every other request's body and decoding holds
// stays below, and HeapSlack. Objects not yet collected count as live; when
// the heap stands in the way, it runs one collection and weighs again, unless
// what is reserved and estimate pass the room with nothing live.
//
// When estimate does not fit it reserves nothing and fails as heapVerdict
// answers: ErrNoHeapRoom, which a decoder answers as too large, only when the
// body provably misses with nothing other requests hold, and ErrHeapBusy, a
// throttle, otherwise.
func ReserveHeapBeside(estimate, own int64) (release func(), err error) {
	err = weighHeap(estimate, own, reserveObjects)
	if err != nil {
		return nil, err
	}

	return func() {
		heap.mu.Lock()
		heap.reserved -= estimate
		heap.releases++
		heap.mu.Unlock()
	}, nil
}

// heapUse is what weighHeap does with n.
type heapUse int

const (
	// checkLive weighs n beside what the last collection found live and
	// reserves nothing (TakeDenseLane's projected heap).
	checkLive heapUse = iota
	// reserveObjects weighs n beside the heap's objects and reserves it when
	// it fits (ReserveHeapBeside).
	reserveObjects
)

// heapWeigh is what one weigh of n bytes decides on.
type heapWeigh struct {
	// n is what the weigh is for, and own the caller's bytes of reserved, the
	// rest of which other requests hold.
	n, own, reserved int64
	// counted is what the weigh counts of the heap: its objects, what the last
	// collection found live, or nothing.
	counted int64
	room    int64
	// floor is a lower bound on what stays live with nothing other requests
	// hold (heapFloor); zero when none can be shown.
	floor int64
	// othersHeld reports that other requests held a reservation at this weigh
	// or an earlier one of the same attempt, or gave one back during it.
	othersHeld bool
}

// heapVerdict is the one answer every heap weigh gives (CheckHeapRoom,
// TakeDenseLane, ReserveHeapBeside): nil when what it counts, what is reserved
// and n fit the room; otherwise ErrNoHeapRoom, too large, when no other
// request held a reservation during the attempt, or when n and own miss the
// room beside the floor, so that the body would miss with nothing other
// requests hold; and ErrHeapBusy, a throttle, when the floor does not show
// that: the body could fit once the others are answered. A wrong throttle
// costs one retry; a wrong too large fails a request that would be served.
func heapVerdict(w heapWeigh) error {
	switch {
	case w.counted+w.reserved+w.n <= w.room:
		return nil
	case !w.othersHeld:
		return ErrNoHeapRoom
	case w.floor+w.own+w.n > w.room:
		return ErrNoHeapRoom
	default:
		return ErrHeapBusy
	}
}

// heapAttempt is one weighHeap: up to two weighs, a collection between them,
// and what the weighs have seen so far.
type heapAttempt struct {
	n, own, room int64
	use          heapUse
	// weighed reports a first weigh, at which releases was read.
	weighed  bool
	releases uint64
	// collected reports a collection run by this attempt, begun when
	// heap.releases was collectedAt.
	collected   bool
	collectedAt uint64
	// othersHeld is sticky: once a weigh sees another request's reservation,
	// or one given back since the first weigh, every later weigh of the
	// attempt is answered as beside others. A collection marks a value live
	// before its request is answered; a later weigh that finds no reservation
	// and no room may be missing for that value alone.
	othersHeld bool
}

// weighHeap weighs n against the room (heapRoom) beside what is reserved, own
// bytes of it the caller's, and the heap, as use says. When the heap stands in
// the way it runs one collection and weighs again, unless what is reserved and
// n pass the room with nothing live. It fails as heapVerdict answers.
func weighHeap(n, own int64, use heapUse) error {
	a := &heapAttempt{n: n, own: own, room: heapRoom(), use: use}

	collectable, err := a.weigh()
	if err != nil && collectable {
		a.collect()

		_, err = a.weigh()
	}

	return err
}

// weigh weighs the attempt's n beside what is reserved and the heap: its
// objects for reserveObjects, which then reserves n when they fit, and what the
// last collection found live for checkLive. On a miss it reports whether what
// is reserved and n would fit were nothing live, whether a collection could
// make room. What the last collection found live does not say what a
// reservation can take: it can have run while an earlier body's value was
// live.
func (a *heapAttempt) weigh() (collectable bool, err error) {
	heap.mu.Lock()
	defer heap.mu.Unlock()

	objects, live := heap.read()

	if !a.weighed {
		a.weighed, a.releases = true, heap.releases
	}

	a.othersHeld = a.othersHeld || heap.reserved > a.own || heap.releases != a.releases

	counted := objects
	if a.use == checkLive {
		counted = live
	}

	err = heapVerdict(heapWeigh{
		n: a.n, own: a.own, reserved: heap.reserved, counted: counted, room: a.room,
		floor: a.floor(live), othersHeld: a.othersHeld,
	})
	if err != nil {
		return heap.reserved+a.n <= a.room, err
	}

	if a.use == reserveObjects {
		heap.reserved += a.n
	}

	return false, nil
}

// collect runs the attempt's one collection, noting the reservations given
// back before it began.
func (a *heapAttempt) collect() {
	heap.mu.Lock()
	a.collected, a.collectedAt = true, heap.releases
	heap.mu.Unlock()

	heap.collect()
}

// floor is heapFloor for the attempt, with heap.mu held: zero unless the
// attempt's own collection found live, with no reservation given back since
// it began.
func (a *heapAttempt) floor(live int64) int64 {
	if !a.collected || heap.releases != a.collectedAt {
		return 0
	}

	return heapFloor(live, heap.reserved-a.own)
}

// heapFloor is a lower bound on what stays live with nothing other requests
// hold, from what a collection found live and what they reserve: a request's
// reservation covers what it keeps live, so the rest stays without them. It
// holds only when no reservation was given back after the collection began: a
// value whose request was answered after it was marked still reads as live.
func heapFloor(live, others int64) int64 {
	return max(live-others, 0)
}

// heapRoom is what ReserveHeapBeside weighs the heap against: the Go memory
// limit, no more than the memory cap (SetMemoryCap), less the in-flight bound
// and HeapSlack. With neither a limit nor a cap it takes SoftLimit(UnknownMemory)
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

// SetMemoryCap makes n the most heapRoom takes as the Go memory limit,
// whatever GOMEMLIMIT set the runtime's to, and returns the cap it replaces.
// cmd/gostack sets it to SoftLimit of the memory its container, or else its
// machine, has. Zero or less removes it.
func SetMemoryCap(n int64) (old int64) {
	return memoryCap.Swap(max(n, 0))
}

// CheckHeapRoom weighs n against the room ReserveHeapBeside weighs the heap
// against beside what is reserved, were nothing else live, and reserves
// nothing: a body it refuses, TakeDenseLane and ReserveHeapBeside refuse too,
// whatever a collection would free. It answers as heapVerdict does with
// nothing counted and a floor of zero: ErrHeapBusy when n passes the room
// beside what other requests reserve but fits it alone, and ErrNoHeapRoom
// when n passes the room alone. It reads what is reserved and the room, never
// the heap, and runs no collection.
func CheckHeapRoom(n int64) error {
	room := heapRoom()

	heap.mu.Lock()
	defer heap.mu.Unlock()

	return heapVerdict(heapWeigh{n: n, reserved: heap.reserved, room: room, othersHeld: heap.reserved > 0})
}

// denseLane is held by the one body at a time that is walked and decoded past
// DecodeBudget (TakeDenseLane).
var denseLane atomic.Bool

var (
	// ErrDenseLaneTaken is TakeDenseLane's refusal while another body holds
	// the lane: the body could be decoded once the lane is free.
	ErrDenseLaneTaken = errors.New("another body holds the dense lane")
	// ErrHeapBusy is the refusal of a body the heap has no room for while
	// other requests hold reservations, when it is not shown to miss with
	// nothing of theirs (heapVerdict): it can fit once they are answered.
	ErrHeapBusy = errors.New("other requests' reservations hold the heap's room")
	// ErrNoHeapRoom is the refusal of a body the heap has no room for with
	// nothing other requests hold: none held a reservation during the
	// weighing, or the floor of what stays without them leaves no room
	// (heapVerdict).
	ErrNoHeapRoom = errors.New("no room on the heap")
)

// TakeDenseLane is ReserveHeapBeside(n, 0) for a body whose decoding passes
// DecodeBudget, the only kind of body decoded off the in-flight bound, and
// whose decoding is projected to keep peak, its own n bytes included: such a
// body first takes the process's one dense lane, so at most one of them is
// walked and decoded at a time and none waits for another. It fails, holding
// nothing, with ErrDenseLaneTaken when another body has the lane, and, as
// heapVerdict answers, with ErrHeapBusy or ErrNoHeapRoom when the heap could
// not hold peak beside what the last collection found live (weighHeap with
// checkLive, which may collect, one lane holder at a time) or has no room for
// n. Otherwise lane gives back the lane, which the decoder does as soon as
// decoding returns, and runs once however often it is called; release gives
// back the reservation, which lasts until the request is answered when a
// HeapKeeper keeps it, so the lane bounds the decodes at once and the
// reservation the heap.
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

// readHeap reads the heap's objects and what the last collection found live,
// with heap.mu held, which guards heapSamples.
func readHeap() (objects, live int64) {
	metrics.Read(heapSamples)

	objects = int64(heapSamples[0].Value.Uint64()) //nolint:gosec // the heap fits an int64
	live = int64(heapSamples[1].Value.Uint64())    //nolint:gosec // the heap fits an int64

	return objects, live
}

var heapSamples = []metrics.Sample{{Name: heapObjects}, {Name: heapLive}}
