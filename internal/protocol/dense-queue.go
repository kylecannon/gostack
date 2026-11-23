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

// SetDenseWait sets how long a dense body waits in the queue (WaitDense) and
// how long the lane's holder waits for the bodies it sheds to give their room
// back (weighHeap). A test that changes it puts the old value back.
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

// denseWaiter is one body in the dense queue, of n bytes of its own. turn
// holds at most one wake, so a wake sent while the head is trying is kept for
// its next wait. heap.mu guards holds and shed.
type denseWaiter struct {
	turn chan struct{}
	n    int64
	// holds reports that n is reserved on the heap for the body's own bytes,
	// in heap.reserved and heap.queued: from joining until its request gives
	// it back, except while the body is the head trying, whose try weighs
	// those bytes as its own, and once it is admitted, when its lane's
	// reservation counts them.
	holds bool
	// shed reports that the lane's holder needs the room the queued bodies
	// take (weighHeap): the body leaves at its next wake with ErrHeapBusy.
	shed bool
}

// WaitDense runs try, a dense body's attempt at the lane and the heap's room,
// in its turn: at once when no body waits, and otherwise once every body ahead
// of it in the queue has left it, so a later body never takes the room a
// waiting one is owed. A try that fails with ErrDenseLaneTaken or ErrHeapBusy,
// room the body could have once another request gives back the lane or a
// reservation, joins the queue, first in first out, and the head tries again
// each time the lane or a reservation is given back. Any other answer of a
// try, admitted or not, is WaitDense's at once: a body found unable to fit
// with nothing other requests hold (ErrNoHeapRoom) waits no further. A body
// that joins a queue waits for its turn before its first try, so a caller
// that can tell a body is too large without the lane (CheckHeapRoom) asks
// before WaitDense.
//
// A body that joins reserves n bytes of heap for its own bytes, which stay
// live while it waits, so every weigh counts them as another request's and
// answers a body they stand in the way of busy, never too large. It joins only
// when what the last collection found live, what is reserved and n fit the
// room, without a collection, and otherwise it is answered ErrHeapBusy at once: a later
// arrival never takes the room an earlier one needs. joined, when not nil,
// takes the reservation's release once the body joins, to run when its
// request is answered (HeapKeeper.KeepHeap); the caller then gives back what
// the body holds of the in-flight bound, since the heap counts its bytes from
// then on. With joined nil the release runs when WaitDense returns. A body
// admitted from the queue holds its lane's reservation instead, and the
// release does nothing.
//
// When ctx ends or the body has waited DefaultDenseWait (SetDenseWait), it
// leaves the queue with the last busy answer, which the decoder answers as a
// throttle. It leaves with ErrHeapBusy, too, when the lane's holder sheds the
// queue for room the queued bodies take (weighHeap).
//
// Each try is a fresh weighing (heapAttempt): what a try saw of other
// requests' reservations, and the floor its collection found, lasts that try
// only, since a reservation given back during the wait makes them stale. Only
// the body at the head of the queue tries, so at most one waiting body weighs
// the heap or collects at a time. Nothing runs while no body waits: the
// waiter and its timer are made only for a body that waits, and the timer is
// stopped when it leaves.
//
// A body passes a waiting one in one window: between its first try and its
// joining the queue, a body that finds the queue empty tries first. The passed
// body loses its turn, and when the passing body holds the lane and misses for
// the passed body's bytes, it sheds the passed body, which is throttled though
// it arrived first.
func WaitDense(ctx context.Context, n int64, joined func(release func()), try func() error) error {
	err := TryDense(try)
	if !denseBusy(err) {
		return err
	}

	w, joinErr := joinDense(n)
	if joinErr != nil {
		return joinErr
	}

	if joined != nil {
		joined(w.release)
	} else {
		defer w.release()
	}

	return w.wait(ctx, try, err)
}

// TryDense runs try once for a dense body that does not wait, one whose
// request holds nothing of the in-flight bound: while any body waits in the
// dense queue, or the lane's holder sheds it, it answers ErrHeapBusy without
// trying, so the body never takes the room a waiting one is owed, and
// otherwise it answers try's answer. It is WaitDense's first step, without
// the queue.
func TryDense(try func() error) error {
	heap.mu.Lock()
	waiting := len(heap.queue) > 0 || heap.shedding
	heap.mu.Unlock()

	if waiting {
		return ErrHeapBusy
	}

	err := try()
	if err != nil {
		// The try may have held the lane for a moment, while the head of a
		// queue that formed meanwhile tried and missed for it.
		wakeDense()
	}

	return err
}

// DenseQueued is how many bodies wait in the dense queue (WaitDense).
func DenseQueued() int {
	heap.mu.Lock()
	defer heap.mu.Unlock()

	return len(heap.queue)
}

// joinDense puts a body of n bytes at the end of the queue with n reserved,
// as WaitDense describes, and gives the head its turn when the body is the
// head, since the room it missed may have been given back before it joined.
// It answers ErrHeapBusy, joining nothing, when the reservation does not fit
// or the lane's holder sheds the queue.
func joinDense(n int64) (*denseWaiter, error) {
	room := heapRoom()

	heap.mu.Lock()
	defer heap.mu.Unlock()

	if heap.shedding {
		return nil, ErrHeapBusy
	}

	if n > 0 {
		// What the last collection found live, as TakeDenseLane weighs the
		// room: the heap's objects count what the last decode left for the
		// collector, and would turn away a body that waits beside nothing.
		_, live := heap.read()
		if live+heap.reserved+n > room {
			return nil, ErrHeapBusy
		}
	}

	w := &denseWaiter{turn: make(chan struct{}, 1), n: n}
	w.hold()
	heap.queue = append(heap.queue, w)

	if heap.queue[0] == w {
		wakeHead()
	}

	return w, nil
}

// wait is WaitDense for a body in the queue: it tries in its turn, and leaves
// when it is admitted, refused, shed, or its wait ends. busy is the answer it
// leaves with when its wait ends first.
func (w *denseWaiter) wait(ctx context.Context, try func() error, busy error) error {
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

		if !w.startTurn() {
			return ErrHeapBusy
		}

		// An admitted body's bytes are its lane's reservation's from here on;
		// any other's are the queue's again until its request gives them back.
		err = try()
		if err != nil {
			w.missTurn()
		}

		if !denseBusy(err) {
			return err
		}
	}
}

// startTurn begins the head's try: false for a body that is shed, and
// otherwise its bytes stop being the queue's for the try, which weighs them
// as its own.
func (w *denseWaiter) startTurn() bool {
	heap.mu.Lock()
	defer heap.mu.Unlock()

	if w.shed {
		return false
	}

	w.unhold()

	return true
}

// missTurn ends a head's try that was not admitted: its bytes are the
// queue's again until its request gives them back. A head whose try ends
// while the lane's holder sheds the queue is shed with it.
func (w *denseWaiter) missTurn() {
	heap.mu.Lock()
	defer heap.mu.Unlock()

	w.hold()

	if heap.shedding {
		w.shed = true

		select {
		case w.turn <- struct{}{}:
		default:
		}
	}
}

// release gives back the body's reservation, when it holds one, and gives the
// head its turn. Its request runs it when it is answered.
func (w *denseWaiter) release() {
	heap.mu.Lock()
	defer heap.mu.Unlock()

	if !w.holds {
		return
	}

	w.unhold()
	heap.releases++
	wakeHead()
}

// hold reserves the body's bytes for the queue, with heap.mu held.
func (w *denseWaiter) hold() {
	w.holds = true
	heap.reserved += w.n
	heap.queued += w.n
}

// unhold gives back what hold reserved, with heap.mu held, and tells the
// lane's holder that sheds the queue once nothing queued is reserved.
func (w *denseWaiter) unhold() {
	w.holds = false
	heap.reserved -= w.n
	heap.queued -= w.n

	if heap.queued == 0 && heap.drained != nil {
		close(heap.drained)
		heap.drained = nil
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

// shedDense makes every queued body leave with ErrHeapBusy and refuses new
// ones (joinDense, TryDense) until endShedding, for the lane's holder, which
// needs the room their bytes take (weighHeap). The head trying, which holds
// nothing of the queue's and may be the holder itself, is not shed. It returns
// a channel closed once no queued body's reservation is held, with heap.mu
// held.
func shedDense() <-chan struct{} {
	for _, w := range heap.queue {
		if !w.holds {
			continue
		}

		w.shed = true

		select {
		case w.turn <- struct{}{}:
		default:
		}
	}

	heap.shedding = true

	if heap.drained == nil {
		heap.drained = make(chan struct{})
	}

	return heap.drained
}

// endShedding lets bodies join the queue again.
func endShedding() {
	heap.mu.Lock()
	heap.shedding = false
	heap.mu.Unlock()
}

// wakeDense gives the head its turn: when the lane is given back, and after a
// refused try by a body outside the queue, which may have held the lane for a
// moment and made the head miss.
//
// heap.mu is the queue's, the reservations' and the weighing's one lock, held
// only to read or change them and never across a collection, a try, a wait or
// a decode. A lane's give-back calls wakeDense inside its sync.Once: the one
// place a lock is taken under another, and nothing takes them the other way.
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
