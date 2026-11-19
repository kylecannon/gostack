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

// lane holds the calls queued for one destination. One worker at most serves a
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
	closed  bool
	group   sync.WaitGroup
}

// laneKey is the destination of a call: the account and region it acts as, the
// service, and the method, path and target of the operation. S3's event
// notifications are SQS SendMessage calls as the bucket's owner to the queue's
// region, so the events one owner sends to one region arrive in the order they
// were raised.
func laneKey(call Call) string {
	return call.Account + "\x00" + call.Region + "\x00" + call.Service + "\x00" + methodOf(call) + "\x00" +
		pathOf(call) + "\x00" + call.Target
}

// Enqueue hands call to a worker and returns at once; done, when it is not nil,
// gets the answer on the worker's goroutine. The queue holds MaxQueued calls
// waiting to start: past that, and after Close, Enqueue refuses the call and
// reports false.
//
// Calls with the same destination (laneKey) run one at a time, in the order
// they were queued. Calls to different destinations run at once, up to
// MaxInFlight, and a worker serves the waiting destinations in turn, one call
// each. A worker starts when a call has none to take it and exits when no call
// is waiting, so nothing runs while idle.
func (c *Client) Enqueue(call Call, done func(Response, error)) bool {
	if c == nil {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed || c.queued >= MaxQueued {
		return false
	}

	c.queued++

	key := laneKey(call)
	if l, ok := c.lanes[key]; ok {
		l.calls = append(l.calls, queued{call: call, done: done})

		return true
	}

	l := &lane{key: key, calls: []queued{{call: call, done: done}}}

	if c.lanes == nil {
		c.lanes = make(map[string]*lane)
	}

	c.lanes[key] = l
	c.waiting = append(c.waiting, l)

	if c.workers < MaxInFlight {
		c.workers++
		c.group.Go(c.work)
	}

	return true
}

// Idle reports whether no call is queued and no worker is running.
func (c *Client) Idle() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.workers == 0 && c.queued == 0
}

// Close drops the calls still queued, cancels the ones in flight and waits for
// the workers to exit. Later Enqueue calls are refused. It is safe to call more
// than once.
func (c *Client) Close() {
	c.mu.Lock()
	c.closed = true
	c.lanes = nil
	c.waiting = nil
	c.queued = 0
	c.mu.Unlock()

	c.cancel()
	c.group.Wait()
}

// work serves the waiting lanes, one call from each in turn, until none waits.
func (c *Client) work() {
	var served *lane

	for {
		l, next, ok := c.next(served)
		if !ok {
			return
		}

		ctx, cancel := context.WithTimeout(c.ctx, callTimeout)
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
func (c *Client) next(served *lane) (*lane, queued, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		c.workers--

		return nil, queued{}, false
	}

	if served != nil {
		if len(served.calls) > 0 {
			c.waiting = append(c.waiting, served)
		} else {
			delete(c.lanes, served.key)
		}
	}

	if len(c.waiting) == 0 {
		c.workers--

		return nil, queued{}, false
	}

	l := c.waiting[0]
	c.waiting[0] = nil
	c.waiting = c.waiting[1:]

	head := l.calls[0]
	l.calls[0] = queued{}
	l.calls = l.calls[1:]
	c.queued--

	return l, head, true
}
