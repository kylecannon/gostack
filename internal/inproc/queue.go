package inproc

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
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
	// hub, when not nil, counted the call when it was queued, and is told
	// when the call is done with or dropped.
	hub *Hub
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
	// hub is the Hub the client is bound to, if any.
	hub *Hub
	// lanes are the lanes with a call waiting or running, by key.
	lanes map[string]*lane
	// waiting are the lanes with a call waiting and no worker, in the order
	// they are to get one.
	waiting []*lane
	queued  int
	workers int
	// running counts the calls a worker is serving: taken, and not yet
	// answered by the handler.
	running int
	closed  bool
	// cut counts the calls that were never served because the queue was shut:
	// those queued when it shut, those running then that were not answered
	// with a 2xx after all, and those Enqueue refused after.
	cut   int
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
// not be made or whose handler panicked or called runtime.Goexit (ErrAborted).
// A done that panics is logged, and the queue goes on. The queue holds MaxQueued calls
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

	// The hub counts the call before any worker can see it, so a call queued
	// by a call the hub is waiting for keeps the hub's count above zero.
	if q.hub != nil {
		q.hub.enter()
	}

	item := queued{call: call, done: done, hub: q.hub}

	key := laneKey(call)
	if l, ok := q.lanes[key]; ok {
		l.calls = append(l.calls, item)

		return true
	}

	l := &lane{key: key, calls: []queued{item}}

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

// Hold counts work of the caller's own in the pending count of the Hub the
// client is bound to, as a queued call counts, until release is called: work
// that is not a call in the queue yet but will queue calls, such as deliveries
// waiting in a lane of the caller's for room or for their turn. Hub.Drain
// waits for it as it waits for a queued call. A client bound to no hub counts
// nothing. release may be called more than once; only the first counts.
func (c *Client) Hold() (release func()) {
	q := &c.queue

	q.mu.Lock()
	hub := q.hub
	q.mu.Unlock()

	if hub == nil {
		return func() {}
	}

	hub.enter()

	var once sync.Once

	return func() { once.Do(hub.leave) }
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
// It counts the calls it dropped or cancelled in cut; a cancelled call that
// its handler still answers with a 2xx is taken off the count when it returns.
// Shutting a queue that is already shut cuts nothing.
func (q *queue) shut() {
	q.mu.Lock()

	if q.closed {
		q.mu.Unlock()

		return
	}

	for _, l := range q.lanes {
		for _, dropped := range l.calls {
			if dropped.hub != nil {
				dropped.hub.leave()
			}
		}
	}

	q.closed = true
	q.cut += q.queued + q.running
	q.lanes = nil
	q.waiting = nil
	q.queued = 0
	q.mu.Unlock()

	q.cancel()
}

// work serves the waiting lanes, one call from each in turn, until none waits.
func (c *Client) work() {
	c.workAfter(nil)
}

// workAfter is work for a worker that takes the place of one that was serving
// lane served, or nil.
func (c *Client) workAfter(served *lane) {
	for {
		l, next, ok := c.queue.next(served)
		if !ok {
			return
		}

		c.serve(l, next)

		served = l
	}
}

// serve makes item, a call of lane l, and answers it. Do turns a handler's
// panic into the call's error, and answer recovers a panic in done, so the
// worker outlives both.
//
// A handler or a done that calls runtime.Goexit ends the worker's goroutine,
// whatever it defers. The deferred calls answer the call all the same, with
// errExited when the handler had not returned, and start a worker in the
// goroutine's place, which puts l back in line as this one would have.
func (c *Client) serve(l *lane, item queued) {
	finished := false

	defer func() {
		if !finished {
			c.queue.group.Go(func() { c.workAfter(l) })
		}
	}()

	// After done, so that a call done queues keeps the hub waiting.
	if item.hub != nil {
		defer item.hub.leave()
	}

	answered := false

	defer func() {
		if !answered {
			c.queue.answered(Response{}, errExited)
			item.answer(Response{}, errExited)
		}
	}()

	resp, err := c.call(item.call)
	answered = true

	c.queue.answered(resp, err)
	item.answer(resp, err)

	finished = true
}

// call makes a queued call, within callTimeout.
func (c *Client) call(call Call) (Response, error) {
	ctx, cancel := context.WithTimeout(c.queue.ctx, callTimeout)
	defer cancel()

	return c.Do(ctx, call)
}

// answer hands the call's answer to done. A done that panics is logged and
// does not end the worker: the lane goes on with its next call.
func (item queued) answer(resp Response, err error) {
	if item.done == nil {
		return
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("panic in the done of a queued call",
				"service", item.call.Service, "method", methodOf(item.call), "path", pathOf(item.call),
				"target", item.call.Target, "panic", fmt.Sprint(recovered), "stack", string(debug.Stack()))
		}
	}()

	item.done(resp, err)
}

// answered is a call's handler returning with resp or err. A call the queue
// was shut while it ran was counted as cut; one answered with a 2xx all the
// same was served.
func (q *queue) answered(resp Response, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.running--

	if q.closed && err == nil && resp.OK() {
		q.cut--
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

	if q.closed {
		q.workers--

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
		q.workers--

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
