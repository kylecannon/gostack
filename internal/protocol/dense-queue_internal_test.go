package protocol

import (
	"context"
	"math/rand/v2"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
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
	checkSeats(t)

	t.Cleanup(func() {
		// A failed test can leave bodies waiting; its context has ended, so
		// they leave. What they held, and a body stuck in its try, is then
		// cleared, so the next test starts from nothing held instead of
		// failing in its turn.
		assert.Eventually(t, func() bool { return DenseQueued() == 0 }, 5*time.Second, time.Millisecond,
			"the test's bodies leave the queue")

		if t.Failed() {
			heap.mu.Lock()
			heap.queue, heap.reserved, heap.queued, heap.shedding = nil, 0, 0, false
			heap.mu.Unlock()
			denseLane.Store(false)
		}

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
	// n is the body's bytes, and peak what its decoding keeps, n included;
	// zero is n.
	n, peak int64
	// hook, when not nil, runs first in each try.
	hook func()
	// seat is the body's in the queue once it joins (joined): it tries the
	// lane from it, as the decoder does.
	seat          *DenseSeat
	lane, release func()
}

// joined takes the body's seat, as the decoder does when it joins the queue.
func (d *denseTurn) joined(seat *DenseSeat) {
	d.seat = seat
}

func (d *denseTurn) try() error {
	if d.hook != nil {
		d.hook()
	}

	peak := max(d.peak, d.n)

	if d.seat != nil {
		var err error

		d.lane, err = d.seat.TakeLane(context.Background(), peak)

		return err
	}

	err := CheckHeapRoom(peak)
	if err != nil {
		return err
	}

	d.lane, d.release, err = TakeDenseLane(d.n, peak)

	return err
}

// giveBack gives back the lane and the reservation the body took.
func (d *denseTurn) giveBack() {
	d.lane()

	if d.release != nil {
		d.release()
	}
}

// requireQueued waits until n bodies wait in the queue.
func requireQueued(t *testing.T, n int) {
	t.Helper()

	require.Eventually(t, func() bool { return DenseQueued() == n }, 10*time.Second, time.Millisecond,
		"%d bodies wait", n)
}

// requireNothingHeld checks that nothing is left of the bodies: no waiter, the
// lane free and nothing reserved.
func requireNothingHeld(t *testing.T) {
	t.Helper()

	assert.Zero(t, DenseQueued(), "no body waits")
	assert.False(t, denseLane.Load(), "the lane is free")

	heap.mu.Lock()
	defer heap.mu.Unlock()

	assert.Zero(t, heap.reserved, "nothing is reserved")
	assert.Zero(t, heap.queued, "no queued body's bytes are reserved")
	assert.False(t, heap.shedding, "no body is shed")
}

// startDense runs WaitDense for body in its own goroutine and sends its answer
// on the returned channel.
func startDense(ctx context.Context, body *denseTurn) <-chan error {
	answer := make(chan error, 1)

	go func() { answer <- WaitDense(ctx, 0, nil, body.try) }()

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
	err = WaitDense(t.Context(), 0, nil, (&denseTurn{n: 50}).try)
	require.ErrorIs(t, err, ErrHeapBusy)
	assert.GreaterOrEqual(t, time.Since(start), wait, "it waited out the deadline")
	assert.Zero(t, DenseQueued())

	other()

	holder := &denseTurn{n: 10}
	require.NoError(t, holder.try())

	err = WaitDense(t.Context(), 0, nil, (&denseTurn{n: 10}).try)
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

	err = WaitDense(t.Context(), 0, nil, (&denseTurn{n: 101}).try)
	require.ErrorIs(t, err, ErrNoHeapRoom)
	assert.Zero(t, DenseQueued())

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

				err := WaitDense(context.Background(), 0, nil, body.try)
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

// Test_WaitDense_RacingReleasesCancelsAndDeadlinesDrainTheQueue: senders whose
// bodies fit only one at a time wait while others give back the lane after a
// random hold, some cancelled at a random moment, with a deadline drawn per run
// that releases race (1 ms, 3 ms) or that no body reaches (a minute). Every
// sender is answered once, served holding the lane or refused holding nothing;
// with the long deadline every sender not cancelled is served, so a waiter left
// asleep while the lane is free shows as one that never is. After, nothing is
// queued or held, a new body is served at once, and no goroutine is left. Run
// it with -race -count.
func Test_WaitDense_RacingReleasesCancelsAndDeadlinesDrainTheQueue(t *testing.T) {
	const senders = 16

	seed := uint64(time.Now().UnixNano())

	rng := rand.New(rand.NewPCG(seed, seed>>1))
	wait := []time.Duration{time.Millisecond, 3 * time.Millisecond, time.Minute}[rng.IntN(3)]
	t.Logf("seed %d, wait %v", seed, wait)
	before := runtime.NumGoroutine()

	emptyHeap(t, wait)

	var (
		wg      sync.WaitGroup
		answers atomic.Int64
	)

	for range senders {
		cancelAfter := time.Duration(rng.IntN(2_000)) * time.Microsecond
		patient := rng.IntN(2) == 0
		hold := time.Duration(rng.IntN(200)) * time.Microsecond

		wg.Go(func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			if !patient {
				stop := time.AfterFunc(cancelAfter, cancel)
				defer stop.Stop()
			}

			body := &denseTurn{n: 60}
			err := WaitDense(ctx, 0, nil, body.try)

			answers.Add(1)

			if err != nil {
				assert.True(t, denseBusy(err), "refused only for what others hold: %v", err)
				assert.Nil(t, body.lane, "a refused body holds no lane")
				assert.False(t, patient && wait == time.Minute, "a patient body is served: %v", err)

				return
			}

			time.Sleep(hold)
			body.giveBack()
		})
	}

	drained := make(chan struct{})

	go func() {
		wg.Wait()
		close(drained)
	}()

	select {
	case <-drained:
	case <-time.After(30 * time.Second):
		t.Fatal("a sender was never answered")
	}

	assert.Equal(t, int64(senders), answers.Load(), "every sender answered once")
	requireNothingHeld(t)

	late := &denseTurn{n: 60}
	require.NoError(t, WaitDense(context.Background(), 0, nil, late.try), "a body after them is served")
	late.giveBack()

	// Polled here, not with assert.Eventually, which runs its condition in a
	// goroutine of its own.
	for deadline := time.Now().Add(10 * time.Second); runtime.NumGoroutine() > before; {
		if time.Now().After(deadline) {
			stacks := make([]byte, 1<<20)
			t.Fatalf("goroutines left: %d, before %d:\n%s", runtime.NumGoroutine(), before,
				stacks[:runtime.Stack(stacks, true)])
		}

		time.Sleep(time.Millisecond)
	}
}

// Test_WaitDense_AWaiterWokenAsItIsCancelledAnswersOnce: the head of the queue
// is cancelled in the same instant the lane is given back, over and over. It
// is either served, holding the lane, or refused, holding nothing; never both
// and never neither, and a turn it leaves unused passes to the body behind it,
// which is served.
func Test_WaitDense_AWaiterWokenAsItIsCancelledAnswersOnce(t *testing.T) {
	emptyHeap(t, time.Minute)

	served, refused := 0, 0

	for range 200 {
		holder := &denseTurn{n: 60}
		require.NoError(t, WaitDense(context.Background(), 0, nil, holder.try))

		ctx, cancel := context.WithCancel(context.Background())
		head := &denseTurn{n: 60}
		headAnswer := startDense(ctx, head)

		requireQueued(t, 1)

		next := &denseTurn{n: 60}
		nextAnswer := startDense(context.Background(), next)

		requireQueued(t, 2)

		start := make(chan struct{})

		var wg sync.WaitGroup

		wg.Go(func() { <-start; cancel() })
		wg.Go(func() { <-start; holder.giveBack() })
		close(start)
		wg.Wait()

		err := <-headAnswer
		if err != nil {
			require.True(t, denseBusy(err), "refused for what the holder held: %v", err)
			require.Nil(t, head.lane, "a refused body holds no lane")

			refused++
		} else {
			served++

			head.giveBack()
		}

		require.NoError(t, <-nextAnswer, "the body behind is served")
		next.giveBack()
		requireNothingHeld(t)
	}

	t.Logf("served %d, refused %d", served, refused)
}

// Test_WaitDense_NoLockAcrossACollectionOrAWait: the heap's lock, the only one
// the queue, the lane and the reservations take, is free while a body's
// weighing collects and while a body waits its turn.
func Test_WaitDense_NoLockAcrossACollectionOrAWait(t *testing.T) {
	emptyHeap(t, time.Minute)

	// 30 bytes stay live whatever is collected, so a body of 60 beside another
	// request's 20 misses, could fit were nothing live, and collects; the floor
	// it finds then fits it, so it is busy for the other's reservation.
	heap.read = func() (int64, int64) { return 30, 30 }

	// Others, the test among them, take the lock a moment; one held across a
	// collection or a wait is never free until it ends.
	lockFree := func() bool {
		for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			if heap.mu.TryLock() {
				heap.mu.Unlock()

				return true
			}
		}

		return false
	}

	var collections atomic.Int64

	heap.collect = func() {
		assert.True(t, lockFree(), "the heap's lock is free during a collection")
		collections.Add(1)
	}

	other, err := ReserveHeapBeside(20, 0)
	require.NoError(t, err)

	big := &denseTurn{n: 60}
	require.ErrorIs(t, big.try(), ErrHeapBusy)
	assert.Positive(t, collections.Load(), "the weighing collected")

	answer := startDense(context.Background(), big)
	requireQueued(t, 1)
	require.True(t, lockFree(), "the heap's lock is free while a body waits")
	requireQueued(t, 1)

	other()
	require.NoError(t, <-answer, "served once the reservation is given back")
	big.giveBack()
	requireNothingHeld(t)
}

// Test_WaitDense_NothingWhileNoBodyWaits: a body admitted with no body waiting
// makes no waiter, no channel and no timer, so nothing is left running or
// armed while the queue is empty.
func Test_WaitDense_NothingWhileNoBodyWaits(t *testing.T) {
	emptyHeap(t, time.Minute)

	admit := func() error { return nil }

	allocs := testing.AllocsPerRun(100, func() {
		_ = WaitDense(context.Background(), 0, nil, admit)
	})

	assert.Zero(t, allocs, "the uncontended path allocates nothing")
	requireNothingHeld(t)
}
