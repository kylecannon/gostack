package protocol

import (
	"context"
	"errors"
	"sync"
	"time"
)

// DefaultDenseWait is the most a request whose body is decoded past
// DecodeBudget waits for the dense lane and the heap's room, from its body's
// first try at them (WithDenseWait), before it is answered with a throttle:
// its wait in the dense queue (WaitDense) and, once it holds the lane, its
// wait for the bodies it sheds to give their room back (weighHeap), together.
// The largest legal dense body, a 15 MB batch, is walked and decoded in about
// 0.4 s, so 2 s lets a body wait out a few such decodes or a handler that
// keeps a reservation, and answers it well inside DefaultBodyWait, the 5 s a
// body waits for the in-flight bound, and an SDK's read timeout (60 s in
// botocore), which it then retries with its backoff.
const DefaultDenseWait = 2 * time.Second

var denseWait = newDuration(DefaultDenseWait)

// SetDenseWait sets the most a request waits for the dense lane and the
// heap's room (DefaultDenseWait). A test that changes it puts the old value
// back.
func SetDenseWait(wait time.Duration) {
	denseWait.Store(int64(wait))
}

// WithDenseWait is ctx ended DefaultDenseWait (SetDenseWait) from now: the one
// deadline of a request's every wait for the dense lane and the heap's room,
// which the request makes at its body's first try and cancels once it is
// answered.
func WithDenseWait(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, time.Duration(denseWait.Load()))
}

// DenseWaiter is a HeapKeeper whose request's context ends its dense body's
// wait in the queue (WaitDense): a client gone, or a server that closes the
// connection at shutdown, takes the body out of the queue.
type DenseWaiter interface {
	// WaitContext is the request's context.
	WaitContext() context.Context
}

// denseAccount is where a dense body's own bytes are reserved on the heap
// once it joins the queue (DenseSeat). Before it joins they are held under
// the in-flight bound, which heapRoom leaves out of the room.
type denseAccount int

const (
	// inQueue: reserved for a body in the queue, or one shed from it whose
	// request is not answered yet, in heap.reserved and heap.queued. The lane's
	// holder may shed it.
	inQueue denseAccount = iota
	// inLane: reserved for the body that holds the lane, in heap.reserved
	// only. No one sheds it.
	inLane
	// givenBack: its request is answered, and nothing is reserved.
	givenBack
)

// DenseSeat is a body's place in the dense queue, and the reservation of its
// n bytes of its own. The bytes are in exactly one account (denseAccount)
// from joining until the request gives them back (Release), and move between
// accounts only under heap.mu, so every weigh counts them once: as another
// request's, beside which a body is busy, never too large, and as the
// holder's own, never twice. heap.mu guards account and shed.
type DenseSeat struct {
	turn chan struct{}
	n    int64
	// account is where n is reserved.
	account denseAccount
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
// A body that joins takes a seat (DenseSeat) that reserves its n bytes on the
// heap, which stay live while it waits. joined, when not nil, takes the seat
// once the body joins: the caller keeps its release (Release) to run when its
// request is answered (HeapKeeper.KeepHeap), gives back what the body holds of
// the in-flight bound, since the heap counts its bytes from then on, and
// tries the lane in its turn from the seat (DenseSeat.TakeLane), so its bytes
// move to the lane's account instead of being reserved twice. With joined nil
// the release runs when WaitDense returns. A body joins only when what the
// last collection found live, what is reserved and n fit the room, without a
// collection, and is otherwise answered ErrHeapBusy at once: a later arrival
// never takes the room an earlier one needs.
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
// seat and its timer are made only for a body that waits, and the timer is
// stopped when it leaves.
//
// The queue is first in first out from joining. Before it joins, a body's
// first try runs when it finds the queue empty, so a body can pass another
// between that body's first try and its joining; when the passing body holds
// the lane and misses for the passed body's bytes, it sheds it, and the passed
// body is throttled though it arrived first.
func WaitDense(ctx context.Context, n int64, joined func(seat *DenseSeat), try func() error) error {
	err := TryDense(try)
	if !denseBusy(err) {
		return err
	}

	seat, joinErr := joinDense(n)
	if joinErr != nil {
		return joinErr
	}

	if joined != nil {
		joined(seat)
	} else {
		defer seat.Release()
	}

	return seat.wait(ctx, try, err)
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

// joinDense puts a body of n bytes at the end of the queue with n reserved in
// its seat, as WaitDense describes, and gives the head its turn when the body
// is the head, since the room it missed may have been given back before it
// joined. It answers ErrHeapBusy, joining nothing, when the reservation does
// not fit or the lane's holder sheds the queue.
func joinDense(n int64) (*DenseSeat, error) {
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

	s := &DenseSeat{turn: make(chan struct{}, 1), n: n, account: inQueue}
	heap.reserved += n
	heap.queued += n
	heap.queue = append(heap.queue, s)
	seatsMoved(s)

	if heap.queue[0] == s {
		wakeHead()
	}

	return s, nil
}

// TakeLane is TakeDenseLane for the body that holds the seat, in its turn
// (WaitDense), whose decoding is projected to keep peak, its own bytes
// included, until ctx ends: its bytes, reserved already, move from the
// queue's account to the lane's while it holds the lane, and back if it is
// not admitted. Before it takes the lane it weighs peak against the room
// beside what is reserved, less what the queue holds (CheckHeapRoom): what
// other requests reserve leaves the body busy, and the bodies queued behind it
// do not, since the lane's holder sheds them for room their bytes take
// (weighHeap). lane gives back the lane, as TakeDenseLane's does; the seat's
// release gives back the reservation.
func (s *DenseSeat) TakeLane(ctx context.Context, peak int64) (lane func(), err error) {
	err = s.checkRoom(peak)
	if err != nil {
		return nil, err
	}

	if !denseLane.CompareAndSwap(false, true) {
		return nil, ErrDenseLaneTaken
	}

	s.move(inQueue, inLane)

	err = weighHeap(ctx, peak-s.n, s.n, checkLive)
	if err != nil {
		s.move(inLane, inQueue)
		denseLane.Store(false)

		return nil, err
	}

	return giveBackLane(), nil
}

// Release gives back the seat's reservation and gives the head its turn, once
// however often it is called. Its request runs it when it is answered.
func (s *DenseSeat) Release() {
	heap.mu.Lock()
	defer heap.mu.Unlock()

	if s.account == givenBack {
		return
	}

	heap.reserved -= s.n
	if s.account == inQueue {
		heap.queued -= s.n
		drainedCheck()
	}

	s.account = givenBack
	heap.releases++
	seatsMoved(s)
	wakeHead()
}

// checkRoom is CheckHeapRoom(peak) for the body that holds the seat, beside
// what is reserved less what the queue holds: its own bytes, which peak
// counts, and the bodies' behind it.
func (s *DenseSeat) checkRoom(peak int64) error {
	room := heapRoom()

	heap.mu.Lock()
	defer heap.mu.Unlock()

	others := heap.reserved - heap.queued

	return heapVerdict(heapWeigh{
		n: peak, reserved: others, room: room, othersHeld: others > 0 || heap.queued > s.n,
	})
}

// move moves the seat's bytes from one account to another, with heap.mu, when
// they are in from.
func (s *DenseSeat) move(from, to denseAccount) {
	heap.mu.Lock()
	defer heap.mu.Unlock()

	if s.account != from {
		return
	}

	s.account = to

	switch {
	case from == inQueue:
		heap.queued -= s.n
		drainedCheck()
	case to == inQueue:
		heap.queued += s.n
	default: // between the lane's account and none: queued is unchanged
	}

	seatsMoved(s)
}

// wait is WaitDense for a body in the queue: it tries in its turn, and leaves
// when it is admitted, refused, shed, or its wait ends. busy is the answer it
// leaves with when its wait ends first.
func (s *DenseSeat) wait(ctx context.Context, try func() error, busy error) error {
	defer leaveDense(s)

	timer := time.NewTimer(time.Duration(denseWait.Load()))
	defer timer.Stop()

	err := busy

	for {
		select {
		case <-s.turn:
		case <-ctx.Done():
			return err
		case <-timer.C:
			return err
		}

		if s.isShed() {
			return ErrHeapBusy
		}

		err = try()
		if !denseBusy(err) {
			return err
		}
	}
}

// isShed reports whether the lane's holder shed the body.
func (s *DenseSeat) isShed() bool {
	heap.mu.Lock()
	defer heap.mu.Unlock()

	return s.shed
}

// drainedCheck tells the lane's holder that sheds the queue once nothing
// queued is reserved, with heap.mu held.
func drainedCheck() {
	if heap.queued == 0 && heap.drained != nil {
		close(heap.drained)
		heap.drained = nil
	}
}

// seatsMoved runs heap.moved for s, whose account changed: a test's check
// of the seats' accounts, when there is one, with heap.mu held.
func seatsMoved(s *DenseSeat) {
	if heap.moved != nil {
		heap.moved(s)
	}
}

// leaveDense takes s out of the queue and, when it was the head, gives the
// next body its turn.
func leaveDense(s *DenseSeat) {
	heap.mu.Lock()
	defer heap.mu.Unlock()

	for i, queued := range heap.queue {
		if queued != s {
			continue
		}

		heap.queue = append(heap.queue[:i], heap.queue[i+1:]...)

		if i == 0 {
			wakeHead()
		}

		return
	}
}

// shedDense makes every body whose bytes are the queue's leave with
// ErrHeapBusy, and refuses new ones (joinDense, TryDense) until endShedding,
// for the lane's holder, which needs the room their bytes take (weighHeap).
// The holder's own bytes are the lane's, so it is never shed. It returns a
// channel closed once no queued body's reservation is held, with heap.mu
// held.
func shedDense() <-chan struct{} {
	for _, s := range heap.queue {
		if s.account != inQueue {
			continue
		}

		s.shed = true

		select {
		case s.turn <- struct{}{}:
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

// giveBackLane is the lane's give-back for the body that took it: it frees the
// lane and gives the dense queue's head its turn, once however often it is
// called.
func giveBackLane() func() {
	var once sync.Once

	return func() {
		once.Do(func() {
			denseLane.Store(false)
			wakeDense()
		})
	}
}

// wakeDense gives the head its turn: when the lane is given back, and after a
// refused try by a body outside the queue, which may have held the lane for a
// moment and made the head miss.
//
// heap.mu is the queue's, the reservations' and the weighing's one lock, held
// only to read or change them and never across a collection, a try, a wait or
// a decode. A lane's give-back calls wakeDense inside its sync.Once: the one
// place a lock is taken under another, and nothing takes them the other way.
// The in-flight pool's lock is never taken under heap.mu: a body that joins
// gives back its pool share after joinDense unlocks, so its bytes are counted
// twice for a moment, never in no account.
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
