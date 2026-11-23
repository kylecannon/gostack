package protocol

import (
	"context"
	"runtime/debug"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// emptyRoom is the heap's room in the dense queue's tests: the bodies' sizes
// are shares of it.
const emptyRoom = 100

// emptyHeap gives the decode budget's heap emptyRoom bytes of room with
// nothing live, read the same at every weigh, for one test, and the dense
// queue a wait of wait. The collection it runs changes nothing, so bodies may
// weigh it at once.
func emptyHeap(t *testing.T, wait time.Duration) {
	t.Helper()

	read, collect, limit, memoryCap := heap.read, heap.collect, debug.SetMemoryLimit(-1), SetMemoryCap(0)
	oldWait := time.Duration(denseWait.Load())

	bound, _ := InFlight()
	if bound == 0 {
		bound = DefaultInFlight
	}

	heap.read = func() (int64, int64) { return 0, 0 }
	heap.collect = func() {}
	debug.SetMemoryLimit(bound + HeapSlack + emptyRoom)
	SetDenseWait(wait)

	t.Cleanup(func() {
		heap.read, heap.collect = read, collect
		debug.SetMemoryLimit(limit)
		SetMemoryCap(memoryCap)
		SetDenseWait(oldWait)
	})
}

// denseTurn is a dense body of n bytes as the decoder tries it in its turn:
// the room with nothing live, then the lane with n reserved. What it took is
// kept for the test to give back.
type denseTurn struct {
	n             int64
	lane, release func()
}

func (d *denseTurn) try() error {
	err := CheckHeapRoom(d.n)
	if err != nil {
		return err
	}

	d.lane, d.release, err = TakeDenseLane(d.n, d.n)

	return err
}

// giveBack gives back the lane and the reservation the body took.
func (d *denseTurn) giveBack() {
	d.lane()
	d.release()
}

// denseQueued is how many bodies wait in the dense queue.
func denseQueued() int {
	heap.mu.Lock()
	defer heap.mu.Unlock()

	return len(heap.queue)
}

// requireQueued waits until n bodies wait in the queue.
func requireQueued(t *testing.T, n int) {
	t.Helper()

	require.Eventually(t, func() bool { return denseQueued() == n }, 10*time.Second, time.Millisecond,
		"%d bodies wait", n)
}

// requireNothingHeld checks that nothing is left of the bodies: no waiter, the
// lane free and nothing reserved.
func requireNothingHeld(t *testing.T) {
	t.Helper()

	assert.Zero(t, denseQueued(), "no body waits")
	assert.False(t, denseLane.Load(), "the lane is free")

	heap.mu.Lock()
	defer heap.mu.Unlock()

	assert.Zero(t, heap.reserved, "nothing is reserved")
}

// startDense runs WaitDense for body in its own goroutine and sends its answer
// on the returned channel.
func startDense(ctx context.Context, body *denseTurn) <-chan error {
	answer := make(chan error, 1)

	go func() { answer <- WaitDense(ctx, body.try) }()

	return answer
}

// Test_WaitDense_FirstInFirstOut: bodies that each fit the room alone but not
// beside another request's reservation wait, and are admitted in the order
// they arrived, one at a time, each once the one before gives back the lane.
// None is throttled.
func Test_WaitDense_FirstInFirstOut(t *testing.T) {
	emptyHeap(t, time.Minute)

	other, err := ReserveHeapBeside(60, 0)
	require.NoError(t, err)

	bodies := []*denseTurn{{n: 50}, {n: 50}, {n: 50}}
	answers := make([]<-chan error, len(bodies))

	for i, body := range bodies {
		answers[i] = startDense(t.Context(), body)
		requireQueued(t, i+1)
	}

	other()

	for i, body := range bodies {
		require.NoError(t, <-answers[i], "body %d is admitted", i)

		for _, later := range answers[i+1:] {
			assert.Empty(t, later, "a later body waits for body %d", i)
		}

		body.giveBack()
	}

	requireNothingHeld(t)
}

// Test_WaitDense_ALaterBodyThatWouldFitWaitsBehindTheHead: the head misses
// beside another request's reservation, and a smaller body that would fit
// beside it arrives later: it waits behind the head and is admitted after it.
func Test_WaitDense_ALaterBodyThatWouldFitWaitsBehindTheHead(t *testing.T) {
	emptyHeap(t, time.Minute)

	other, err := ReserveHeapBeside(60, 0)
	require.NoError(t, err)

	head, later := &denseTurn{n: 50}, &denseTurn{n: 30}

	headAnswer := startDense(t.Context(), head)
	requireQueued(t, 1)

	laterAnswer := startDense(t.Context(), later)
	requireQueued(t, 2)

	assert.Never(t, func() bool { return len(laterAnswer) > 0 }, 100*time.Millisecond, time.Millisecond,
		"the later body that would fit does not pass the head")

	other()

	require.NoError(t, <-headAnswer)
	assert.Empty(t, laterAnswer, "the head holds the lane")

	head.giveBack()
	require.NoError(t, <-laterAnswer)

	later.giveBack()
	requireNothingHeld(t)
}

// Test_WaitDense_TheDeadlineAnswersBusy: a body that waits past
// DefaultDenseWait (SetDenseWait) leaves the queue with the busy answer it
// waited on, which the decoder answers as a throttle: ErrHeapBusy beside a
// reservation, ErrDenseLaneTaken behind the lane. It holds nothing after.
func Test_WaitDense_TheDeadlineAnswersBusy(t *testing.T) {
	const wait = 50 * time.Millisecond

	emptyHeap(t, wait)

	other, err := ReserveHeapBeside(60, 0)
	require.NoError(t, err)

	start := time.Now()
	err = WaitDense(t.Context(), (&denseTurn{n: 50}).try)
	require.ErrorIs(t, err, ErrHeapBusy)
	assert.GreaterOrEqual(t, time.Since(start), wait, "it waited out the deadline")
	assert.Zero(t, denseQueued())

	other()

	holder := &denseTurn{n: 10}
	require.NoError(t, holder.try())

	err = WaitDense(t.Context(), (&denseTurn{n: 10}).try)
	require.ErrorIs(t, err, ErrDenseLaneTaken)

	holder.giveBack()
	requireNothingHeld(t)
}

// Test_WaitDense_ACancelledBodyLeavesTheQueue: a body whose request's context
// ends leaves the queue at once with a busy answer, the head and a body behind
// it alike, and the bodies after it keep their turns.
func Test_WaitDense_ACancelledBodyLeavesTheQueue(t *testing.T) {
	emptyHeap(t, time.Minute)

	other, err := ReserveHeapBeside(60, 0)
	require.NoError(t, err)

	headCtx, cancelHead := context.WithCancel(t.Context())
	middleCtx, cancelMiddle := context.WithCancel(t.Context())

	head, middle, last := &denseTurn{n: 50}, &denseTurn{n: 50}, &denseTurn{n: 50}

	headAnswer := startDense(headCtx, head)
	requireQueued(t, 1)

	middleAnswer := startDense(middleCtx, middle)
	requireQueued(t, 2)

	lastAnswer := startDense(t.Context(), last)
	requireQueued(t, 3)

	cancelMiddle()
	require.ErrorIs(t, <-middleAnswer, ErrHeapBusy)
	requireQueued(t, 2)

	cancelHead()
	require.ErrorIs(t, <-headAnswer, ErrHeapBusy)
	requireQueued(t, 1)

	other()
	require.NoError(t, <-lastAnswer, "the last body has its turn once the room is given back")

	last.giveBack()
	requireNothingHeld(t)
}

// Test_WaitDense_ABodyThatCannotFitIsAnsweredAtOnce: a body past the room
// with nothing other requests hold is answered as too large at once, beside a
// reservation too, and never waits.
func Test_WaitDense_ABodyThatCannotFitIsAnsweredAtOnce(t *testing.T) {
	emptyHeap(t, time.Minute)

	other, err := ReserveHeapBeside(60, 0)
	require.NoError(t, err)

	err = WaitDense(t.Context(), (&denseTurn{n: 101}).try)
	require.ErrorIs(t, err, ErrNoHeapRoom)
	assert.Zero(t, denseQueued())

	other()
	requireNothingHeld(t)
}

// Test_WaitDense_NoLostWakeups: bodies that fit two at a time, from many
// senders in closed loops, take the lane and hold their reservation a moment
// each: every one is admitted, with a deadline no body reaches, so a wake lost
// between a miss and the wait shows as a body that never is. Nothing is left
// held or queued after. Run it with -race -count.
func Test_WaitDense_NoLostWakeups(t *testing.T) {
	const (
		senders = 8
		rounds  = 200
	)

	emptyHeap(t, time.Minute)

	var wg sync.WaitGroup

	errs := make(chan error, senders*rounds)

	for range senders {
		wg.Go(func() {
			for range rounds {
				body := &denseTurn{n: 40}

				err := WaitDense(context.Background(), body.try)
				if err != nil {
					errs <- err

					continue
				}

				body.lane()
				time.Sleep(10 * time.Microsecond)
				body.release()
			}
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("a body that fits alone was answered %v", err)
	}

	requireNothingHeld(t)
}

// Test_WaitDense_NothingWhileNoBodyWaits: a body admitted with no body waiting
// makes no waiter, no channel and no timer, so nothing is left running or
// armed while the queue is empty.
func Test_WaitDense_NothingWhileNoBodyWaits(t *testing.T) {
	emptyHeap(t, time.Minute)

	admit := func() error { return nil }

	allocs := testing.AllocsPerRun(100, func() {
		_ = WaitDense(context.Background(), admit)
	})

	assert.Zero(t, allocs, "the uncontended path allocates nothing")
	requireNothingHeld(t)
}
