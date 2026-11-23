package awsjson

import (
	"context"
	"math"
	"reflect"
	"runtime/debug"
	"runtime/metrics"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/heaptest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// denseValue is shaped like an AttributeValue list: each element of L decodes
// into far more than the three bytes it takes on the wire.
type denseValue struct {
	L []*denseValue `json:"L,omitempty"`
	S *string       `json:"S,omitempty"`
}

// What a recordingHold records.
const (
	stepHold        = "hold"
	stepKeep        = "keep"
	stepReleasePool = "release pool"
)

// recordingHold is a protocol.HeapKeeper that records what a decoder asks of
// it, in order, and keeps the reservations it is given. When pool is not nil,
// it is what the request holds of the in-flight bound, which ReleasePool
// gives back.
type recordingHold struct {
	steps []string
	kept  []func()
	pool  *protocol.DecodeHold
}

func (h *recordingHold) PoolHeld() int64 {
	return h.pool.Held()
}

func (h *recordingHold) HoldDecode(int64) (int64, error) {
	h.steps = append(h.steps, stepHold)

	return math.MaxInt64, nil
}

func (h *recordingHold) KeepHeap(release func()) bool {
	h.steps = append(h.steps, stepKeep)
	h.kept = append(h.kept, release)

	return true
}

func (h *recordingHold) ReleasePool() {
	h.steps = append(h.steps, stepReleasePool)
	h.pool.Release()
}

func (h *recordingHold) release() {
	for _, release := range h.kept {
		release()
	}

	h.kept = nil
}

// denseBody is a denseValue list of n empty elements.
func denseBody(n int) []byte {
	return []byte(`{"L":[` + strings.Repeat(`{},`, n-1) + `{}]}`)
}

// withMemoryLimit sets the Go memory limit for one test.
func withMemoryLimit(t *testing.T, limit int64) {
	t.Helper()

	old := debug.SetMemoryLimit(limit)
	t.Cleanup(func() { debug.SetMemoryLimit(old) })
}

// Test_DecodeWithin_PastTheBudgetMovesTheBodyOffThePool: a body whose walk
// passes protocol.DecodeBudget, for a service with a larger budget, has its
// bytes reserved on the heap and gives back what it holds of the in-flight
// bound right there, where a service at protocol.DecodeFactor refuses it, and
// only then reserves the heap the walk bounds decoding to keep. A body within
// DecodeBudget is charged to the pool as before, and nothing is moved.
func Test_DecodeWithin_PastTheBudgetMovesTheBodyOffThePool(t *testing.T) {
	withMemoryLimit(t, 8<<30)

	body := denseBody(300_000)
	budget := protocol.DecodeBudgetAt(len(body), 256)

	cost, _, err := decodeCost(body, planOf(reflect.TypeFor[denseValue]()), budget, protocol.FreeDecode, nil)
	require.Nil(t, err)
	require.Greater(t, cost, protocol.DecodeBudget(len(body)), "the body is past DecodeBudget")

	hold := &recordingHold{}
	t.Cleanup(hold.release)

	v, _, err := DecodeWithin[denseValue](body, budget, hold)
	require.Nil(t, err)
	assert.Len(t, v.L, 300_000)
	assert.Equal(t, []string{stepKeep, stepReleasePool, stepKeep}, hold.steps,
		"the body's bytes are kept on the heap and the pool given back before the estimate is reserved")

	within := denseBody(100_000)
	cost, _, err = decodeCost(within, planOf(reflect.TypeFor[denseValue]()), budget, protocol.FreeDecode, nil)
	require.Nil(t, err)
	require.Greater(t, cost, int64(protocol.FreeDecode))
	require.LessOrEqual(t, cost, protocol.DecodeBudget(len(within)), "the body is within DecodeBudget")

	hold = &recordingHold{}
	t.Cleanup(hold.release)

	_, _, err = DecodeWithin[denseValue](within, budget, hold)
	require.Nil(t, err)
	assert.Equal(t, []string{stepHold}, hold.steps, "a body within DecodeBudget is charged to the pool only")
}

// Test_DecodeWithin_ARefusedMoveIsMainsAnswer: a body past DecodeBudget whose
// own bytes find no room on the heap is refused with ErrTooLarge, as a service
// at protocol.DecodeFactor refuses it, keeping its pool charge until the
// request is released and reserving nothing; at protocol.DecodeFactor the same
// body is refused without a move.
func Test_DecodeWithin_ARefusedMoveIsMainsAnswer(t *testing.T) {
	body := denseBody(300_000)

	bound, _ := protocol.InFlight()
	if bound == 0 {
		bound = protocol.DefaultInFlight
	}

	// No room past the in-flight bound and HeapSlack for the test's own heap.
	withMemoryLimit(t, bound+protocol.HeapSlack+1)

	hold := &recordingHold{}

	_, _, err := DecodeWithin[denseValue](body, protocol.DecodeBudgetAt(len(body), 256), hold)
	assert.Same(t, ErrTooLarge, err)
	assert.Empty(t, hold.steps, "nothing is reserved and the pool is kept")

	withMemoryLimit(t, 8<<30)

	_, _, err = DecodeWithin[denseValue](body, protocol.DecodeBudget(len(body)), hold)
	assert.Same(t, ErrTooLarge, err)
	assert.Empty(t, hold.steps, "at DecodeFactor nothing is moved")
}

// requireLaneFree fails unless the dense lane is free now.
func requireLaneFree(t *testing.T) {
	t.Helper()

	lane, release, err := protocol.TakeDenseLane(0, 0)
	require.NoError(t, err, "the dense lane is free")
	lane()
	release()
}

// plainHold is a protocol.DecodeHolder that keeps nothing: DecodeWithin gives
// back its reservations once the body is decoded.
type plainHold struct{}

func (plainHold) HoldDecode(int64) (int64, error) { return math.MaxInt64, nil }

// Test_DecodeWithin_OneDenseBodyAtATime: while another body holds the dense
// lane, a body past DecodeBudget is answered with a throttle, which an SDK
// retries, at once and holding nothing.
// Once the lane is free it is served, and gives the lane back when it is
// decoded while its request keeps its reservations, so a second dense body
// is served while the first is still being answered. Only a body whose walk
// passes DecodeBudget reserves heap, so only one taking the lane does: a body
// not walked costs at most FreeDecode past its own bytes twice, under
// DecodeSlack.
func Test_DecodeWithin_OneDenseBodyAtATime(t *testing.T) {
	withMemoryLimit(t, 8<<30)
	require.Less(t, int64(protocol.FreeDecode), int64(protocol.DecodeSlack),
		"a body that is not walked is within DecodeBudget")

	body := denseBody(300_000)
	budget := protocol.DecodeBudgetAt(len(body), 256)

	lane, release, laneErr := protocol.TakeDenseLane(0, 0)
	require.NoError(t, laneErr)

	refused := &recordingHold{}
	t.Cleanup(refused.release)

	_, _, err := DecodeWithin[denseValue](body, budget, refused)
	assert.Equal(t, protocol.ThrottledJSON(), err, "the lane is taken: a throttle")
	assert.Empty(t, refused.steps, "nothing is reserved and the pool is kept")

	lane()
	release()

	first := &recordingHold{}
	t.Cleanup(first.release)

	_, _, err = DecodeWithin[denseValue](body, budget, first)
	require.Nil(t, err, "the lane is free again")
	require.Equal(t, []string{stepKeep, stepReleasePool, stepKeep}, first.steps)
	assert.Len(t, first.kept, 2, "the request keeps both reservations")
	requireLaneFree(t)

	second := &recordingHold{}
	t.Cleanup(second.release)

	_, _, err = DecodeWithin[denseValue](body, budget, second)
	require.Nil(t, err, "served while the first request is still answered")
	assert.Equal(t, []string{stepKeep, stepReleasePool, stepKeep}, second.steps)

	first.release()
	second.release()
	requireLaneFree(t)
}

// Test_DecodeWithin_TheLaneIsFreeAfterEveryPath: a body that took the dense
// lane gives it back as soon as DecodeWithin returns, whatever happens after
// the move, while a HeapKeeper still keeps its reservations: a walk that then
// passes the service's budget, a body that is not valid json, and a holder
// that keeps nothing, decoded or refused.
func Test_DecodeWithin_TheLaneIsFreeAfterEveryPath(t *testing.T) {
	withMemoryLimit(t, 8<<30)

	body := denseBody(300_000)

	hold := &recordingHold{}
	t.Cleanup(hold.release)

	_, _, err := DecodeWithin[denseValue](body, protocol.DecodeBudgetAt(len(body), 3), hold)
	assert.Same(t, ErrTooLarge, err, "past the service's own budget after the move")
	assert.Equal(t, []string{stepKeep, stepReleasePool}, hold.steps)
	requireLaneFree(t)

	broken := body[:len(body)-2]
	budget := protocol.DecodeBudgetAt(len(broken), 256)

	hold = &recordingHold{}
	t.Cleanup(hold.release)

	_, _, err = DecodeWithin[denseValue](broken, budget, hold)
	require.NotNil(t, err, "not valid json")
	assert.Equal(t, []string{stepKeep, stepReleasePool, stepKeep}, hold.steps)
	requireLaneFree(t)

	_, _, err = DecodeWithin[denseValue](body, protocol.DecodeBudgetAt(len(body), 256), plainHold{})
	require.Nil(t, err)
	requireLaneFree(t)

	_, _, err = DecodeWithin[denseValue](broken, budget, plainHold{})
	require.NotNil(t, err)
	requireLaneFree(t)
}

// Test_DecodeWithin_ABodyTheHeapCouldNotHoldIsRefusedAtTheMark: a body past
// DecodeBudget whose own bytes fit the heap, but not with the heap its walk
// is projected to end at, is refused where the walk passes DecodeBudget,
// holding nothing and keeping the pool, instead of being walked to its end
// and refused by the final reservation. With room for both it is served.
func Test_DecodeWithin_ABodyTheHeapCouldNotHoldIsRefusedAtTheMark(t *testing.T) {
	heaptest.Settle(t) // the room below is built on what is live

	body := denseBody(3_000_000)
	budget := protocol.DecodeBudgetAt(len(body), 256)

	_, estimate, err := decodeCost(body, planOf(reflect.TypeFor[denseValue]()), budget, protocol.FreeDecode, nil)
	require.Nil(t, err)
	require.Greater(t, estimate, int64(4*len(body)), "the body's heap is far past its bytes")

	bound, _ := protocol.InFlight()
	if bound == 0 {
		bound = protocol.DefaultInFlight
	}

	sample := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	metrics.Read(sample)

	live := int64(sample[0].Value.Uint64())

	// Room for the body's bytes and half its heap, past what is live now.
	withMemoryLimit(t, bound+protocol.HeapSlack+live+int64(len(body))+estimate/2)

	hold := &recordingHold{}
	t.Cleanup(hold.release)

	_, _, err = DecodeWithin[denseValue](body, budget, hold)
	assert.Same(t, ErrTooLarge, err, "main's answer")
	assert.Empty(t, hold.steps, "refused at the mark: nothing is reserved and the pool is kept")
	requireLaneFree(t)

	withMemoryLimit(t, 8<<30)

	hold = &recordingHold{}
	t.Cleanup(hold.release)

	_, _, err = DecodeWithin[denseValue](body, budget, hold)
	require.Nil(t, err, "room for both: served")
	assert.Equal(t, []string{stepKeep, stepReleasePool, stepKeep}, hold.steps)
}

// Test_DecodeWithin_NoDenseBodyWhileABodyWaits: while a request waits for
// room under the in-flight bound, a body past DecodeBudget is answered with a
// throttle at once, holding nothing, so that the queue drains as fast as it
// does when such bodies are refused. Once nothing waits, the same body is
// served.
func Test_DecodeWithin_NoDenseBodyWhileABodyWaits(t *testing.T) {
	withMemoryLimit(t, 8<<30)

	bound, _ := protocol.InFlight()

	t.Cleanup(func() {
		protocol.SetInFlight(bound)
		protocol.SetBodyWait(protocol.DefaultBodyWait)
	})

	protocol.SetInFlight(protocol.DefaultInFlight)
	protocol.SetBodyWait(time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// The first hold takes the bound less one body; the second waits for it.
	var first, second protocol.DecodeHold

	_, err := first.Cover(ctx, 1<<40)
	require.NoError(t, err)

	waited := make(chan error, 1)

	go func() {
		_, err := second.Cover(ctx, 1<<40)
		waited <- err
	}()

	require.Eventually(t, func() bool { return protocol.BodiesWait(0) }, 10*time.Second, time.Millisecond,
		"the second hold waits for the bound")

	body := denseBody(300_000)
	budget := protocol.DecodeBudgetAt(len(body), 256)

	refused := &recordingHold{}
	t.Cleanup(refused.release)

	_, _, decodeErr := DecodeWithin[denseValue](body, budget, refused)
	assert.Equal(t, protocol.ThrottledJSON(), decodeErr, "a body waits for the bound: a throttle")
	assert.Empty(t, refused.steps, "nothing is reserved and the pool is kept")

	first.Release()
	require.NoError(t, <-waited, "the waiting hold is granted")
	second.Release()

	served := &recordingHold{}
	t.Cleanup(served.release)

	_, _, decodeErr = DecodeWithin[denseValue](body, budget, served)
	require.Nil(t, decodeErr, "nothing waits: the dense body is served")
	assert.Equal(t, []string{stepKeep, stepReleasePool, stepKeep}, served.steps)

	served.release()
	requireLaneFree(t)
}

// Test_DecodeWithin_ABodyWaitingOnlyOnTheDenseBodysBytesDoesNotStopIt: a
// request that waits for room under the in-flight bound only because a body
// past DecodeBudget holds it does not stop that body: the body is moved off
// the bound, which lets the waiting request in, and served. Refusing it would
// answer a legal request with a terminal 400 for want of bytes it holds itself.
func Test_DecodeWithin_ABodyWaitingOnlyOnTheDenseBodysBytesDoesNotStopIt(t *testing.T) {
	withMemoryLimit(t, 8<<30)

	bound, _ := protocol.InFlight()

	t.Cleanup(func() {
		protocol.SetInFlight(bound)
		protocol.SetBodyWait(protocol.DefaultBodyWait)
	})

	protocol.SetInFlight(protocol.DefaultInFlight)
	protocol.SetBodyWait(time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// The dense body's request holds the bound less one body; the other
	// request waits for as much, which only the dense body's bytes free.
	var dense, waiting protocol.DecodeHold

	_, err := dense.Cover(ctx, 1<<40)
	require.NoError(t, err)
	require.Positive(t, dense.Held())

	waited := make(chan error, 1)

	go func() {
		_, err := waiting.Cover(ctx, 1<<40)
		waited <- err
	}()

	t.Cleanup(waiting.Release)

	require.Eventually(t, func() bool { return protocol.BodiesWait(0) }, 10*time.Second, time.Millisecond,
		"the other request waits for the bound")

	body := denseBody(300_000)
	budget := protocol.DecodeBudgetAt(len(body), 256)

	served := &recordingHold{pool: &dense}
	t.Cleanup(served.release)

	_, _, decodeErr := DecodeWithin[denseValue](body, budget, served)
	require.Nil(t, decodeErr, "the waiter waits only on the dense body's bytes: served")
	assert.Equal(t, []string{stepKeep, stepReleasePool, stepKeep}, served.steps)
	require.NoError(t, <-waited, "moving the dense body lets the waiting request in")

	served.release()
	requireLaneFree(t)
}

// withHeapRoom gives the decode budget's heap room bytes, past the in-flight
// bound and protocol.HeapSlack, with no Go memory limit, for one test.
func withHeapRoom(t *testing.T, room int64) {
	t.Helper()

	bound, _ := protocol.InFlight()
	if bound == 0 {
		bound = protocol.DefaultInFlight
	}

	withMemoryLimit(t, math.MaxInt64)

	old := protocol.SetMemoryCap(room + bound + protocol.HeapSlack)
	t.Cleanup(func() { protocol.SetMemoryCap(old) })
}

// Test_DecodeWithin_ADenseBodyThatFitsAloneIsThrottledBesideAnothersReservation:
// a dense body that fits the heap's room alone, of a request that holds none
// of the in-flight bound and so does not wait in the dense queue
// (Test_DecodeWithin_AChargedBodyWaitsForTheRoom has one that does), sent
// while an earlier dense request is still answered and keeps its reservation
// and its value live, is
// answered with a throttle, which an SDK retries, not refused as too large;
// once the earlier request is answered the same body is served. A reservation
// held with nothing of it live, the lane given back, throttles it too.
func Test_DecodeWithin_ADenseBodyThatFitsAloneIsThrottledBesideAnothersReservation(t *testing.T) {
	heaptest.Settle(t)

	body := denseBody(3_000_000)
	budget := protocol.DecodeBudgetAt(len(body), 256)
	live, peak := HeapEstimate[denseValue](body)
	need := int64(len(body)) + live + peak

	withHeapRoom(t, need*3/2) // room for one body, not two

	alone := &recordingHold{}
	_, _, err := DecodeWithin[denseValue](body, budget, alone)
	require.Nil(t, err, "alone the body is served")
	alone.release()

	first := &recordingHold{}
	t.Cleanup(first.release)

	value, _, err := DecodeWithin[denseValue](body, budget, first)
	require.Nil(t, err)

	second := &recordingHold{}
	t.Cleanup(second.release)

	_, _, err = DecodeWithin[denseValue](body, budget, second)
	assert.Equal(t, protocol.ThrottledJSON(), err, "the first request is still answered: a throttle")
	assert.Empty(t, second.steps, "nothing is reserved and the pool is kept")

	require.Len(t, value.L, 3_000_000, "the first value is live until its request is answered")
	first.release() // the first value is garbage from here on

	third := &recordingHold{}
	t.Cleanup(third.release)

	_, _, err = DecodeWithin[denseValue](body, budget, third)
	require.Nil(t, err, "the first request answered: served")
	third.release()

	// Another body holds the lane and as much as the body needs, nothing of
	// it live; then gives back the lane and keeps the reservation.
	lane, reservation, laneErr := protocol.TakeDenseLane(need, 0)
	require.NoError(t, laneErr)

	release := sync.OnceFunc(reservation)
	t.Cleanup(release)
	t.Cleanup(lane)

	held := &recordingHold{}
	t.Cleanup(held.release)

	_, _, err = DecodeWithin[denseValue](body, budget, held)
	assert.Equal(t, protocol.ThrottledJSON(), err, "the lane and the reservation held: a throttle")

	lane()

	_, _, err = DecodeWithin[denseValue](body, budget, held)
	assert.Equal(t, protocol.ThrottledJSON(), err, "the reservation held: a throttle")
	assert.Empty(t, held.steps)

	release()

	_, _, err = DecodeWithin[denseValue](body, budget, held)
	require.Nil(t, err, "the reservation given back: served")
	held.release()
	requireLaneFree(t)
}

// Test_DecodeWithin_ABodyPastTheRoomAloneIsTooLargeBesideAnothersReservation:
// a dense body that would not fit the heap's room with nothing else reserved
// or live is refused as too large, as it is alone, while another request
// holds a reservation: no answer of other requests would let it in, so it is
// not throttled.
func Test_DecodeWithin_ABodyPastTheRoomAloneIsTooLargeBesideAnothersReservation(t *testing.T) {
	heaptest.Settle(t)

	body := denseBody(3_000_000)
	budget := protocol.DecodeBudgetAt(len(body), 256)
	live, peak := HeapEstimate[denseValue](body)
	need := int64(len(body)) + live + peak

	withHeapRoom(t, need/2)

	alone := &recordingHold{}
	t.Cleanup(alone.release)

	_, _, err := DecodeWithin[denseValue](body, budget, alone)
	require.Same(t, ErrTooLarge, err, "alone the body is too large")

	other, err2 := protocol.ReserveHeapBeside(need/8, 0)
	require.NoError(t, err2)
	t.Cleanup(other)

	beside := &recordingHold{}
	t.Cleanup(beside.release)

	_, _, err = DecodeWithin[denseValue](body, budget, beside)
	assert.Same(t, ErrTooLarge, err, "too large beside another request's reservation too")
	assert.Empty(t, beside.steps, "nothing is reserved and the pool is kept")
	requireLaneFree(t)
}
