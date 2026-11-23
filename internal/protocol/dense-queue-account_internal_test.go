package protocol

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// checkSeats checks, for one test, at each move of a seat's account and under
// heap.mu, that the seats' bytes are counted once: each seat whose request is
// not answered is in one account, what the queue's account holds is
// heap.queued, and what both hold is reserved. emptyHeap runs it, so every
// dense-queue test does.
func checkSeats(t *testing.T) {
	t.Helper()

	seats := map[*DenseSeat]struct{}{}

	heap.mu.Lock()
	heap.moved = func(s *DenseSeat) {
		if s.account == givenBack {
			delete(seats, s)
		} else {
			seats[s] = struct{}{}
		}

		var queued, laned int64

		for seat := range seats {
			switch seat.account {
			case inQueue:
				queued += seat.n
			case inLane:
				laned += seat.n
			default:
			}
		}

		if queued != heap.queued {
			t.Errorf("the queue's account holds %d bytes of seats, heap.queued %d", queued, heap.queued)
		}

		if queued+laned > heap.reserved {
			t.Errorf("the seats hold %d bytes, more than the %d reserved", queued+laned, heap.reserved)
		}
	}
	heap.mu.Unlock()

	t.Cleanup(func() {
		heap.mu.Lock()
		heap.moved = nil
		heap.mu.Unlock()
	})
}

// answerWithin is the answer on answer, or a failure when none comes within
// seconds: far less than the queue's wait in these tests, a minute.
func answerWithin(t *testing.T, answer <-chan error, what string) error {
	t.Helper()

	select {
	case err := <-answer:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: no answer within 5s", what)

		return nil
	}
}

// Test_WaitDense_TheHeadIsNotStarvedByTheBodiesBehindIt: SQ-532 round-2 F1,
// the reviewer's input. With a room of 100, the holder holds 10, the head has
// 10 bytes and a peak of 90, and a body of 15 waits behind it. Once the holder
// gives everything back, the head fits alone, and only the body behind it
// stands in its way: the head sheds it and is decoded, without waiting out
// the queue's wait.
func Test_WaitDense_TheHeadIsNotStarvedByTheBodiesBehindIt(t *testing.T) {
	emptyHeap(t, time.Minute)

	holder := &denseTurn{n: 10}
	require.NoError(t, holder.try())

	head := &queuedBody{n: 10, peak: 90}
	headAnswer := head.start(t.Context())
	requireQueued(t, 1)

	behind := &queuedBody{n: 15}
	behindAnswer := behind.start(t.Context())
	requireQueued(t, 2)

	holder.giveBack()

	require.ErrorIs(t, answerWithin(t, behindAnswer, "the body behind the head"), ErrHeapBusy,
		"the later arrival is shed")
	behind.release()

	require.NoError(t, answerWithin(t, headAnswer, "the head"), "the head fits alone: it is decoded")

	head.release()
	head.giveBack()
	requireNothingHeld(t)
}

// Test_WaitDense_TheHeadsBytesAreAlwaysCounted: SQ-532 round-2 F2, forced.
// The head's try runs while the holder holds the lane, and from inside it,
// the window where a5bd299a counted the head's bytes in no account, a body
// joins and the holder weighs its final reservation. The head's 30 bytes are
// live and still another request's: the join that would pass the room beside
// them is refused, and the holder's weigh that misses for them is busy, never
// too large.
func Test_WaitDense_TheHeadsBytesAreAlwaysCounted(t *testing.T) {
	var live atomic.Int64

	live.Store(40)
	liveHeap(t, &live, live.Load)

	holder := &denseTurn{n: 10}
	require.NoError(t, holder.try())

	reserved := make(chan error, 1)

	var (
		final  func()
		tries  atomic.Int64
		joined atomic.Bool
	)

	head := &queuedBody{n: 30}
	head.hook = func() {
		if tries.Add(1) != 2 {
			return
		}

		w, err := joinDense(45)
		if err == nil {
			joined.Store(true)
			leaveDense(w)
			w.Release()
		}

		go func() {
			var err error

			final, err = ReserveHeapBeside(55, holder.n)
			reserved <- err
		}()

		select {
		case err := <-reserved:
			reserved <- err
		case <-time.After(100 * time.Millisecond):
		}
	}

	headAnswer := head.start(t.Context())
	requireQueued(t, 1)

	// A reservation given back gives the head its turn while the holder
	// holds the lane.
	other, err := ReserveHeapBeside(0, 0)
	require.NoError(t, err)
	other()

	require.Eventually(t, func() bool { return tries.Load() >= 2 }, 5*time.Second, time.Millisecond)

	select {
	case err := <-reserved:
		require.NotErrorIs(t, err, ErrNoHeapRoom, "the holder misses for the head's bytes: busy, never too large")
		reserved <- err
	default:
	}

	assert.False(t, joined.Load(), "a body of 45 does not fit beside the 40 live and the head's 30 reserved")

	require.ErrorIs(t, answerWithin(t, headAnswer, "the head"), ErrHeapBusy, "the holder sheds the head")

	live.Store(10)
	head.release()

	require.NoError(t, answerWithin(t, reserved, "the holder"),
		"the holder is admitted once the head's bytes are given back, and never too large")

	final()
	holder.giveBack()
	requireNothingHeld(t)
}
