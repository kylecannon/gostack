package protocol

import (
	"context"
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// liveHeap is emptyHeap, with a wait of a minute, and live bytes on the heap:
// every weigh reads live as the heap's objects and what the last collection
// found, and a collection sets live to what collect returns. The holder's
// bytes and a waiting body's stay live until the test says otherwise.
func liveHeap(t *testing.T, live *atomic.Int64, collect func() int64) {
	t.Helper()
	emptyHeap(t, time.Minute)

	heap.read = func() (int64, int64) { return live.Load(), live.Load() }
	heap.collect = func() { live.Store(collect()) }
}

// queuedBody is a dense body of n bytes that joins the queue as the decoder
// has it join: its reservation's release is kept, to run when the test
// answers its request.
type queuedBody struct {
	denseTurn

	release func()
}

// start runs WaitDense for the body in its own goroutine, keeping the
// reservation's release, and sends its answer on the returned channel. The
// test fails within seconds when the body is answered without joining, or
// neither joins nor is answered.
func (q *queuedBody) start(t *testing.T) <-chan error {
	t.Helper()

	answer := make(chan error, 1)
	kept := make(chan func(), 1)

	go func() {
		answer <- WaitDense(t.Context(), q.n, func(seat *DenseSeat) {
			q.joined(seat)
			kept <- seat.Release
		}, q.try)
	}()

	select {
	case q.release = <-kept:
		// Its request is answered at the latest when the test ends, so a
		// failed test leaves nothing reserved; the release runs once.
		t.Cleanup(q.release)
	case err := <-answer:
		t.Fatalf("the body of %d bytes was answered %v without joining the queue", q.n, err)
	case <-time.After(5 * time.Second):
		t.Fatalf("the body of %d bytes neither joined the queue nor was answered within 5s", q.n)
	}

	return answer
}

// Test_WaitDense_AQueuedBodysBytesAreAnotherRequests: a body that joins the
// queue reserves its own bytes, so a weigh it stands in the way of is busy,
// a throttle, never too large, and its release gives them back once.
func Test_WaitDense_AQueuedBodysBytesAreAnotherRequests(t *testing.T) {
	emptyHeap(t, time.Minute)

	holder := &denseTurn{n: 10}
	require.NoError(t, holder.try())

	waiter := &queuedBody{n: 30}
	answer := waiter.start(t)
	requireQueued(t, 1)

	require.ErrorIs(t, CheckHeapRoom(80), ErrHeapBusy, "80 bytes fit alone: the waiter's 30 make them busy")
	require.ErrorIs(t, CheckHeapRoom(101), ErrNoHeapRoom, "101 bytes never fit")

	holder.giveBack()
	require.NoError(t, <-answer, "the waiter is admitted once the lane is free")

	heap.mu.Lock()
	assert.Equal(t, int64(30), heap.reserved, "the admitted body's seat is the lane's reservation, reserved once")
	assert.Zero(t, heap.queued, "and no longer the queue's")
	heap.mu.Unlock()

	waiter.release()
	waiter.release()

	heap.mu.Lock()
	assert.Zero(t, heap.reserved, "its request gives its bytes back once")
	heap.mu.Unlock()

	waiter.giveBack()
	requireNothingHeld(t)
}

// Test_WaitDense_ABodyWhoseBytesDoNotFitIsNotQueued: a body that misses its
// first try and whose own bytes do not fit the room beside what is live and
// reserved is answered busy at once, a throttle, holding nothing: a later
// arrival never queues into the room the lane's holder needs.
func Test_WaitDense_ABodyWhoseBytesDoNotFitIsNotQueued(t *testing.T) {
	var live atomic.Int64

	live.Store(40)
	liveHeap(t, &live, live.Load)

	holder := &denseTurn{n: 10}
	require.NoError(t, holder.try())

	joined := false
	err := WaitDense(t.Context(), 55, func(*DenseSeat) { joined = true }, (&denseTurn{n: 55}).try)
	require.ErrorIs(t, err, ErrHeapBusy)
	assert.False(t, joined, "it never joined")
	assert.Zero(t, DenseQueued())

	holder.giveBack()
	requireNothingHeld(t)
}

// Test_WaitDense_TheLaneHolderShedsAQueueThatTakesItsRoom: SQ-529's schedule
// with the heap in the test's hands. The holder of the lane has 10 bytes live
// and reserved; a body of 30 arrives after it and waits, its bytes live and
// reserved. The holder's final reservation of 55 fits the room with the
// waiter's bytes gone, and not beside them. The waiter, the later arrival, is
// answered busy; the holder is not admitted while the waiter's bytes are
// reserved, and is once the waiter's request gives them back and a collection
// frees them. No body is answered too large.
func Test_WaitDense_TheLaneHolderShedsAQueueThatTakesItsRoom(t *testing.T) {
	var (
		live     atomic.Int64
		answered atomic.Bool
	)

	live.Store(40)
	liveHeap(t, &live, func() int64 {
		if answered.Load() {
			return 10
		}

		return live.Load()
	})

	holder := &denseTurn{n: 10}
	require.NoError(t, holder.try())

	waiter := &queuedBody{n: 30}
	waiterAnswer := waiter.start(t)
	requireQueued(t, 1)

	reserved := make(chan error, 1)

	var final func()

	go func() {
		var err error

		final, err = ReserveHeapBeside(55, holder.n)
		reserved <- err
	}()

	require.ErrorIs(t, answerWithin(t, waiterAnswer, "the waiter"), ErrHeapBusy, "the later arrival is shed")
	assert.Zero(t, DenseQueued())
	assert.Never(t, func() bool { return len(reserved) > 0 }, 100*time.Millisecond, time.Millisecond,
		"the holder is not admitted while the waiter's bytes are reserved")

	late := &queuedBody{n: 5}
	err := WaitDense(t.Context(), late.n, func(*DenseSeat) { t.Error("no body joins while the queue is shed") }, late.try)
	require.ErrorIs(t, err, ErrHeapBusy, "a body that arrives while the queue is shed is answered busy")

	answered.Store(true)
	waiter.release()

	require.NoError(t, answerWithin(t, reserved, "the holder"),
		"the holder is admitted once the waiter's bytes are given back")

	final()
	holder.giveBack()
	requireNothingHeld(t)
}

// Test_WaitDense_AHeadTryingWhenTheQueueIsShedIsShedWithIt: the head of the
// queue is trying, holding nothing of the queue's, when the lane's holder
// sheds the body behind it. The head's try misses, and its bytes are the
// queue's again: it is shed too, so the holder waits for its request, not for
// the wait to end.
func Test_WaitDense_AHeadTryingWhenTheQueueIsShedIsShedWithIt(t *testing.T) {
	var (
		live     atomic.Int64
		answered atomic.Bool
	)

	live.Store(40)
	liveHeap(t, &live, func() int64 {
		if answered.Load() {
			return 10
		}

		return live.Load()
	})

	holder := &denseTurn{n: 10}
	require.NoError(t, holder.try())

	trying, missed := make(chan struct{}), make(chan struct{})
	tries := 0
	headTry := func() error {
		tries++
		if tries == 2 {
			close(trying)
			<-missed
		}

		return ErrDenseLaneTaken
	}

	headAnswer, headKept := make(chan error, 1), make(chan func(), 1)

	go func() {
		headAnswer <- WaitDense(t.Context(), 5, func(seat *DenseSeat) { headKept <- seat.Release }, headTry)
	}()

	headRelease := <-headKept
	<-trying

	behind := &queuedBody{n: 30}
	behindAnswer := behind.start(t)
	requireQueued(t, 2)

	reserved := make(chan error, 1)

	var final func()

	go func() {
		var err error

		final, err = ReserveHeapBeside(55, holder.n)
		reserved <- err
	}()

	require.ErrorIs(t, answerWithin(t, behindAnswer, "the body behind the head"),
		ErrHeapBusy, "the body behind the head is shed")
	close(missed)
	require.ErrorIs(t, answerWithin(t, headAnswer, "the head"),
		ErrHeapBusy, "the head that missed while the queue is shed is shed")

	answered.Store(true)
	behind.release()
	assert.Never(t, func() bool { return len(reserved) > 0 }, 50*time.Millisecond, time.Millisecond,
		"the holder is not admitted while the head's bytes are reserved")
	headRelease()

	require.NoError(t, answerWithin(t, reserved, "the holder"),
		"the holder is admitted once both bodies' bytes are given back")

	final()
	holder.giveBack()
	requireNothingHeld(t)
}

// Test_WaitDense_TheLaneHolderWaitsForAShedBodyAtMostTheWait: a shed body
// whose request is never answered keeps its bytes; the holder waits for them
// DefaultDenseWait (SetDenseWait) and is then answered busy, a throttle.
func Test_WaitDense_TheLaneHolderWaitsForAShedBodyAtMostTheWait(t *testing.T) {
	const wait = 50 * time.Millisecond

	var live atomic.Int64

	live.Store(40)
	liveHeap(t, &live, live.Load)

	holder := &denseTurn{n: 10}
	require.NoError(t, holder.try())

	waiter := &queuedBody{n: 30}
	waiterAnswer := waiter.start(t)
	requireQueued(t, 1)

	SetDenseWait(wait)

	start := time.Now()
	_, err := ReserveHeapBeside(55, holder.n)
	require.ErrorIs(t, err, ErrHeapBusy)
	assert.GreaterOrEqual(t, time.Since(start), wait, "it waited for the shed body")
	require.ErrorIs(t, answerWithin(t, waiterAnswer, "the waiter"), ErrHeapBusy)

	waiter.release()
	holder.giveBack()
	requireNothingHeld(t)
}

// Test_WaitDense_TheLaneHolderDoesNotShedForAnotherRequestsRoom: the holder's
// reservation misses for another request's reservation, room the queued
// body's bytes do not stand in the way of. The holder is answered busy and
// the queued body keeps its turn.
func Test_WaitDense_TheLaneHolderDoesNotShedForAnotherRequestsRoom(t *testing.T) {
	var live atomic.Int64

	live.Store(20)
	liveHeap(t, &live, live.Load)

	other, err := ReserveHeapBeside(40, 0)
	require.NoError(t, err)

	holder := &denseTurn{n: 10}
	require.NoError(t, holder.try())

	waiter := &queuedBody{n: 5}
	waiterAnswer := waiter.start(t)
	requireQueued(t, 1)

	_, err = ReserveHeapBeside(45, holder.n)
	require.ErrorIs(t, err, ErrHeapBusy, "busy for the other request's 40")
	requireQueued(t, 1)
	assert.Empty(t, waiterAnswer, "the waiter keeps its turn")

	other()
	holder.giveBack()
	require.NoError(t, answerWithin(t, waiterAnswer, "the waiter"))

	waiter.release()
	waiter.giveBack()
	requireNothingHeld(t)
}

// Test_WaitDense_RacingShedsReleasesAndCancelsDrainTheQueue: senders of 30
// bytes join the queue with their bytes reserved, as the decoder has them
// join, and give the reservation back when their request is answered, after a
// random delay. An admitted sender holds the lane and makes a final
// reservation of 45 beside its 30, which misses beside a queued body and
// sheds the queue. Some are cancelled at a random moment, and the deadline is
// drawn per run: one releases race (1 ms, 3 ms) or one no body reaches (a
// minute). Every sender is answered once and none too large: room the others
// hold is busy, a throttle. After, nothing is queued, reserved or shed, a new
// body is served at once, and no goroutine is left. Run it with -race -count.
func Test_WaitDense_RacingShedsReleasesAndCancelsDrainTheQueue(t *testing.T) {
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
		cancels := rng.IntN(2) == 0
		hold := time.Duration(rng.IntN(200)) * time.Microsecond

		wg.Go(func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			if cancels {
				stop := time.AfterFunc(cancelAfter, cancel)
				defer stop.Stop()
			}

			body := &denseTurn{n: 30}
			release := func() {}
			err := WaitDense(ctx, body.n, func(seat *DenseSeat) { body.joined(seat); release = seat.Release }, body.try)

			answers.Add(1)
			// The request is answered after hold, and gives back the queue's
			// reservation then.
			defer func() {
				time.Sleep(hold)
				release()
			}()

			if err != nil {
				assert.True(t, denseBusy(err), "refused only for what others hold: %v", err)
				assert.Nil(t, body.lane, "a refused body holds no lane")

				return
			}

			final, err := ReserveHeapBeside(45, body.n)
			if err != nil {
				assert.True(t, denseBusy(err), "the holder's final reservation is never too large: %v", err)
			} else {
				final()
			}

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

	late := &denseTurn{n: 30}
	require.NoError(t, WaitDense(context.Background(), late.n, nil, late.try), "a body after them is served")
	late.giveBack()
	requireNothingHeld(t)

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
