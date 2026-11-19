package inproc

import (
	"context"
	"sync"
	"time"
)

const (
	// MaxQueued is how many calls Enqueue holds, waiting to start, before it
	// refuses the next.
	MaxQueued = 4096

	// MaxInFlight is how many queued calls run at once. A call is served in
	// memory on this process's own CPUs, so running more at once than there are
	// cores gains nothing, and 16 covers the cores gostack is given. The bound
	// keeps a burst of queued calls, a bulk delete raising thousands of S3
	// events for one, from taking a goroutine each and crowding out the
	// requests clients are waiting on.
	MaxInFlight = 16

	// callTimeout bounds one queued call.
	callTimeout = 30 * time.Second
)

// queued is a call waiting for a worker, and what to do with its answer.
type queued struct {
	call Call
	done func(Response, error)
}

// lane holds the calls queued under one lane key. One worker at most serves a
// lane, one call at a time, so its calls run in the order they were queued.
type lane struct {
	key   string
	calls []queued
}

// queue is the Enqueue side of a Client.
type queue struct {
	// ctx ends when Close is called.
	ctx    context.Context //nolint:containedctx // the lifetime of the workers, not a request's.
	cancel context.CancelFunc

	mu sync.Mutex
	// lanes are the lanes with a call waiting or running, by key.
	lanes map[string]*lane
	// waiting are the lanes with a call waiting and no worker, in the order
	// they are to get one.
	waiting []*lane
	queued  int
	workers int
	// running counts the calls a worker has taken and not yet answered.
	running int
	closed  bool
	// cut counts the calls that were never served because the queue was shut:
	// those queued or running when it shut, and those Enqueue refused after.
	cut int
	// idle, when not nil, is closed when the last worker exits.
	idle  chan struct{}
	group sync.WaitGroup
}

// laneKey is the lane a call is queued in. It is the call's Lane when the
// caller named one. Otherwise it is the call's destination, where it goes and
// not who sends it: the region, the service, and the method, path and target
// of the operation. The account is not part of it, so calls from different
// accounts to one destination still run in the order they were queued.
//
// The destination is only what the request line and the target say. A json
// call that names its resource in the body, such as an SQS SendMessage, shares
// one lane with every other call of that operation in the region, and two
// operations on one resource, a PUT and then a DELETE of one S3 key, are two
// lanes that can finish in either order. A caller that needs a finer or a
// coarser order sets Lane.
func laneKey(call Call) string {
	if call.Lane != "" {
		return call.Lane
	}

	return call.Region + "\x00" + call.Service + "\x00" + methodOf(call) + "\x00" + pathOf(call) + "\x00" +
		call.Target
}

// Enqueue hands call to a worker and returns at once; done, when it is not nil,
// gets the answer on the worker's goroutine, or the error of a call that could
// not be made or whose handler panicked. The queue holds MaxQueued calls
// waiting to start: past that, and after Close, Enqueue refuses the call and
// reports false.
//
// Calls in one lane (laneKey: the call's Lane, or else its destination) run one
// at a time, in the order they were queued. Calls in different lanes run at
// once, up to MaxInFlight, and a worker serves the waiting lanes in turn, one
// call each. A worker starts when a call has none to take it and exits when no
// call is waiting, so nothing runs while idle.
func (c *Client) Enqueue(call Call, done func(Response, error)) bool {
	if c == nil {
		return false
	}

	q := &c.queue

	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		q.cut++

		return false
	}

	if q.queued >= MaxQueued {
		return false
	}

	q.queued++

	key := laneKey(call)
	if l, ok := q.lanes[key]; ok {
		l.calls = append(l.calls, queued{call: call, done: done})

		return true
	}

	l := &lane{key: key, calls: []queued{{call: call, done: done}}}

	if q.lanes == nil {
		q.lanes = make(map[string]*lane)
	}

	q.lanes[key] = l
	q.waiting = append(q.waiting, l)

	if q.workers < MaxInFlight {
		q.workers++
		q.group.Go(c.work)
	}

	return true
}

// Idle reports whether no call is queued and no worker is running.
func (c *Client) Idle() bool {
	q := &c.queue

	q.mu.Lock()
	defer q.mu.Unlock()

	return q.workers == 0 && q.queued == 0
}

// Close drops the calls still queued, cancels the ones in flight and waits for
// the workers to exit. Later Enqueue calls are refused. It is safe to call more
// than once. A dropped call's done is never called.
func (c *Client) Close() {
	c.queue.shut()
	c.queue.group.Wait()
}

// shut is Close without the wait: it drops the calls still queued, without
// answering them, cancels the ones in flight, and refuses later Enqueue calls.
// It counts the calls it dropped or cancelled in cut, and returns how many.
// Shutting a queue that is already shut cuts nothing.
func (q *queue) shut() int {
	q.mu.Lock()

	if q.closed {
		q.mu.Unlock()

		return 0
	}

	cut := q.queued + q.running

	q.closed = true
	q.lanes = nil
	q.waiting = nil
	q.queued = 0
	q.cut += cut
	q.mu.Unlock()

	q.cancel()

	return cut
}

// idleSignal returns a channel that is closed once no call is queued and no
// worker runs: at once when that is so now.
func (q *queue) idleSignal() <-chan struct{} {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.idle == nil {
		q.idle = make(chan struct{})
	}

	idle := q.idle

	if q.workers == 0 && q.queued == 0 {
		close(q.idle)
		q.idle = nil
	}

	return idle
}

// exit is a worker leaving, under q.mu. The last one out signals idle.
func (q *queue) exit() {
	q.workers--

	if q.workers == 0 && q.idle != nil {
		close(q.idle)
		q.idle = nil
	}
}

// work serves the waiting lanes, one call from each in turn, until none waits.
// Do turns a handler's panic into the call's error, so a worker outlives it.
func (c *Client) work() {
	var served *lane

	for {
		l, next, ok := c.queue.next(served)
		if !ok {
			return
		}

		ctx, cancel := context.WithTimeout(c.queue.ctx, callTimeout)
		resp, err := c.Do(ctx, next.call)

		cancel()

		if next.done != nil {
			next.done(resp, err)
		}

		served = l
	}
}

// next puts served, the lane the worker has just served a call of, back in line
// when it has more calls, then pops the oldest call of the lane whose turn it
// is. When no lane waits, or after Close, the worker is gone, under the same
// lock, so a call queued a moment later starts a new one. A worker started for
// a lane that another worker took first finds none and exits.
func (q *queue) next(served *lane) (*lane, queued, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if served != nil {
		q.running--
	}

	if q.closed {
		q.exit()

		return nil, queued{}, false
	}

	if served != nil {
		if len(served.calls) > 0 {
			q.waiting = append(q.waiting, served)
		} else {
			delete(q.lanes, served.key)
		}
	}

	if len(q.waiting) == 0 {
		q.exit()

		return nil, queued{}, false
	}

	l := q.waiting[0]
	q.waiting[0] = nil
	q.waiting = q.waiting[1:]

	head := l.calls[0]
	l.calls[0] = queued{}
	l.calls = l.calls[1:]
	q.queued--
	q.running++

	return l, head, true
}
