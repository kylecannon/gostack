package protocol

import (
	"context"
	"errors"
	"time"
)

// DefaultDenseWait is how long a body decoded past DecodeBudget waits in the
// dense queue (WaitDense) for the lane and the heap's room before its request
// is answered with a throttle. The largest legal dense body, a 15 MB batch, is
// walked and decoded in about 0.4 s, so 2 s lets a body wait out a few such
// decodes or a handler that keeps a reservation, and answers it well inside
// DefaultBodyWait, the 5 s a body waits for the in-flight bound, and an SDK's
// read timeout (60 s in botocore), which it then retries with its backoff.
const DefaultDenseWait = 2 * time.Second

var denseWait = newDuration(DefaultDenseWait)

// SetDenseWait sets how long a dense body waits in the queue (WaitDense). A
// test that changes it puts the old value back.
func SetDenseWait(wait time.Duration) {
	denseWait.Store(int64(wait))
}

// DenseWaiter is a HeapKeeper whose request's context ends its dense body's
// wait in the queue (WaitDense): a client gone, or a server that closes the
// connection at shutdown, takes the body out of the queue.
type DenseWaiter interface {
	// WaitContext is the request's context.
	WaitContext() context.Context
}

// denseWaiter is one body in the dense queue. turn holds at most one wake, so a
// wake sent while the head is trying is kept for its next wait.
type denseWaiter struct {
	turn chan struct{}
}

// WaitDense runs try, a dense body's attempt at the lane and the heap's room,
// in its turn: at once when no body waits, and otherwise once every body ahead
// of it in the queue has left it, so a later body never takes the room a
// waiting one is owed. A try that fails with ErrDenseLaneTaken or ErrHeapBusy,
// room the body could have once another request gives back the lane or a
// reservation, waits in the queue, first in first out, and the head tries
// again each time the lane or a reservation is given back. Any other answer of
// a try, admitted or not, is WaitDense's at once: a body found unable to fit
// with nothing other requests hold (ErrNoHeapRoom) waits no further. A body
// that joins a queue waits for its turn before its first try, so a caller
// that can tell a body is too large without the lane (CheckHeapRoom) asks
// before WaitDense. When ctx
// ends or the body has waited DefaultDenseWait (SetDenseWait), it leaves the
// queue with the last busy answer, which the decoder answers as a throttle.
//
// Each try is a fresh weighing (heapAttempt): what a try saw of other
// requests' reservations, and the floor its collection found, lasts that try
// only, since a reservation given back during the wait makes them stale. Only
// the body at the head of the queue tries, so at most one waiting body weighs
// the heap or collects at a time. Nothing runs while no body waits: the timer
// is made only for a body that waits, and stopped when it leaves.
func WaitDense(ctx context.Context, try func() error) error {
	heap.mu.Lock()
	waiting := len(heap.queue) > 0
	heap.mu.Unlock()

	err := ErrHeapBusy

	if !waiting {
		err = try()
		if err != nil {
			// The try may have held the lane for a moment, while the head
			// of a queue that formed meanwhile tried and missed for it.
			wakeDense()
		}

		if !denseBusy(err) {
			return err
		}
	}

	return queueDense(ctx, try, err)
}

// queueDense is WaitDense for a body that waits its turn: it joins the queue,
// and tries at once when it is the head, since the room it missed may have
// been given back before it joined, and otherwise when the body ahead of it
// leaves. Joining wakes no other body. busy is the answer it leaves with when
// its wait ends first.
func queueDense(ctx context.Context, try func() error, busy error) error {
	w := &denseWaiter{turn: make(chan struct{}, 1)}

	heap.mu.Lock()
	heap.queue = append(heap.queue, w)

	if heap.queue[0] == w {
		wakeHead()
	}

	heap.mu.Unlock()

	defer leaveDense(w)

	timer := time.NewTimer(time.Duration(denseWait.Load()))
	defer timer.Stop()

	err := busy

	for {
		select {
		case <-w.turn:
		case <-ctx.Done():
			return err
		case <-timer.C:
			return err
		}

		err = try()
		if !denseBusy(err) {
			return err
		}
	}
}

// leaveDense takes w out of the queue and, when it was the head, gives the
// next body its turn.
func leaveDense(w *denseWaiter) {
	heap.mu.Lock()
	defer heap.mu.Unlock()

	for i, queued := range heap.queue {
		if queued != w {
			continue
		}

		heap.queue = append(heap.queue[:i], heap.queue[i+1:]...)

		if i == 0 {
			wakeHead()
		}

		return
	}
}

// wakeDense gives the head its turn: when the lane is given back, and after a
// refused try by a body outside the queue, which may have held the lane for a
// moment and made the head miss.
func wakeDense() {
	heap.mu.Lock()
	wakeHead()
	heap.mu.Unlock()
}

// wakeHead gives the body at the head of the queue its turn, with heap.mu
// held. It runs whenever the lane or a reservation is given back, when the
// head leaves, and for a body that joins an empty queue.
func wakeHead() {
	if len(heap.queue) == 0 {
		return
	}

	select {
	case heap.queue[0].turn <- struct{}{}:
	default:
	}
}

// denseBusy reports whether err is room a body could have once other requests
// give back the lane or a reservation: the answers a dense body waits on.
func denseBusy(err error) bool {
	return errors.Is(err, ErrDenseLaneTaken) || errors.Is(err, ErrHeapBusy)
}
