package sqs

import (
	"encoding/base64"
	"strconv"
	"time"
)

// entryState is where a message of a standard queue is.
type entryState int

const (
	// stateDelayed is a message that was sent with a delay that is not over.
	stateDelayed entryState = iota + 1
	// stateVisible is a message that a receive can take.
	stateVisible
	// stateInflight is a message that a receive took and whose visibility
	// timeout is not over.
	stateInflight
)

// entry is a message in a standardContents, with what changes while it is
// there.
type entry struct {
	msg   *message
	seq   uint64
	state entryState
	// deadline is when a delayed message becomes visible and when an in-flight
	// one does again. It is zero for a visible message.
	deadline      time.Time
	receiveCount  int
	firstReceived time.Time
	handles       []string
	// visibleAt and deadlineAt are the positions in the two heaps, -1 when out.
	visibleAt, deadlineAt int
}

// standardContents is the Contents of a standard queue.
//
// Messages are in one of three places: a heap of the visible ones, in the order
// they were sent, and a second heap that holds the delayed and the in-flight
// ones by deadline. One time.Timer is armed for the earliest deadline of that
// second heap and for nothing else: an empty heap leaves no timer armed, so a
// queue with no pending deadline costs no wakeups. The callback and every
// method that reads the heaps first move the messages whose deadline has
// passed, so a late timer changes nothing but how soon a long poll hears of it.
//
// Retention works the same way without a timer: the visible heap is in sending
// order, so the messages past their retention period are at its top, and
// anything that looks at the queue removes them first.
type standardContents struct {
	q        *Queue
	settings func() Settings

	visible   entryHeap
	deadlines entryHeap
	receipts  map[string]*entry
	seq       uint64
	delayed   int
	inflight  int

	// wake is closed and cleared when a message may have become visible, and
	// made again when a long poll asks for it.
	wake   chan struct{}
	closed bool

	timer *time.Timer
	// armed is whether timer is set; armedFor is the deadline it is set for.
	armed    bool
	armedFor time.Time
}

var _ Contents = (*standardContents)(nil)

func newStandardContents(q *Queue, settings func() Settings) *standardContents {
	return &standardContents{
		q:        q,
		settings: settings,
		visible: entryHeap{
			less:  func(a, b *entry) bool { return a.seq < b.seq },
			index: func(e *entry) *int { return &e.visibleAt },
		},
		deadlines: entryHeap{
			less: func(a, b *entry) bool {
				if a.deadline.Equal(b.deadline) {
					return a.seq < b.seq
				}

				return a.deadline.Before(b.deadline)
			},
			index: func(e *entry) *int { return &e.deadlineAt },
		},
		receipts: make(map[string]*entry),
	}
}

func (c *standardContents) Counts() Counts {
	c.refresh(time.Now())

	return Counts{Visible: c.visible.len(), NotVisible: c.inflight, Delayed: c.delayed}
}

func (c *standardContents) Clear() {
	c.visible.reset()
	c.deadlines.reset()
	clear(c.receipts)
	c.delayed, c.inflight = 0, 0
	c.rearm(time.Now())
}

func (c *standardContents) Close() {
	c.Clear()
	c.closed = true
	c.notify()
}

func (c *standardContents) Put(m *message, delay time.Duration) {
	c.seq++
	e := &entry{msg: m, seq: c.seq, visibleAt: -1, deadlineAt: -1}

	if delay > 0 {
		e.state = stateDelayed
		e.deadline = m.sent.Add(delay)
		c.deadlines.push(e)
		c.delayed++
		c.rearm(m.sent)

		return
	}

	c.makeVisible(e)
}

func (c *standardContents) Receive(maxMessages int, visibility time.Duration) []delivery {
	now := time.Now()
	c.refresh(now)

	var taken []*entry

	for len(taken) < maxMessages {
		e := c.visible.pop()
		if e == nil {
			break
		}

		taken = append(taken, e)
	}

	out := make([]delivery, 0, len(taken))

	for _, e := range taken {
		e.receiveCount++
		if e.firstReceived.IsZero() {
			e.firstReceived = now
		}

		handle := newReceiptHandle(c.q.arn, e.msg.id, now)
		e.handles = append(e.handles, handle)
		c.receipts[handle] = e
		out = append(out, delivery{msg: e.msg, handle: handle, receiveCount: e.receiveCount, firstReceived: e.firstReceived})

		if visibility <= 0 {
			c.makeVisible(e)

			continue
		}

		e.state = stateInflight
		e.deadline = now.Add(visibility)
		c.deadlines.push(e)
		c.inflight++
	}

	c.rearm(now)

	return out
}

func (c *standardContents) Delete(handle string) {
	e, ok := c.receipts[handle]
	if !ok {
		return
	}

	c.remove(e)
	c.rearm(time.Now())
}

func (c *standardContents) ChangeVisibility(handle string, visibility time.Duration) bool {
	e, ok := c.receipts[handle]
	if !ok {
		return false
	}

	now := time.Now()
	c.refresh(now)

	// A message that is visible again is not in flight, and the call leaves it
	// alone.
	if e.state != stateInflight {
		return true
	}

	if visibility <= 0 {
		c.deadlines.remove(e)
		c.inflight--
		c.makeVisible(e)
	} else {
		e.deadline = now.Add(visibility)
		c.deadlines.fix(e)
	}

	c.rearm(now)

	return true
}

func (c *standardContents) Wake() <-chan struct{} {
	if c.closed {
		return nil
	}

	if c.wake == nil {
		c.wake = make(chan struct{})
	}

	return c.wake
}

// notify wakes every long poll that asked for the channel.
func (c *standardContents) notify() {
	if c.wake != nil {
		close(c.wake)
		c.wake = nil
	}
}

// makeVisible puts e where a receive finds it and wakes the long polls.
func (c *standardContents) makeVisible(e *entry) {
	e.state = stateVisible
	e.deadline = time.Time{}
	c.visible.push(e)
	c.notify()
}

// remove forgets e and every receipt handle that names it.
func (c *standardContents) remove(e *entry) {
	switch e.state {
	case stateVisible:
		c.visible.remove(e)
	case stateInflight:
		c.deadlines.remove(e)
		c.inflight--
	case stateDelayed:
		c.deadlines.remove(e)
		c.delayed--
	default:
		// An entry that was never put has no place to leave.
	}

	for _, handle := range e.handles {
		delete(c.receipts, handle)
	}

	e.handles = nil
}

// refresh brings the queue up to now: the messages whose delay or visibility
// timeout is over become visible, and the ones past retention are dropped.
func (c *standardContents) refresh(now time.Time) {
	for top := c.deadlines.peek(); top != nil && !top.deadline.After(now); top = c.deadlines.peek() {
		c.deadlines.remove(top)

		if top.state == stateDelayed {
			c.delayed--
		} else {
			c.inflight--
		}

		c.makeVisible(top)
	}

	c.sweepRetention(now)
}

// sweepRetention drops the visible messages that are older than the queue's
// MessageRetentionPeriod, when the setting is on. A message in flight or still
// delayed stays until it is visible, as in LocalStack.
func (c *standardContents) sweepRetention(now time.Time) {
	if !c.settings().EnableMessageRetentionPeriod {
		return
	}

	retention := time.Duration(c.q.intAttr(attrMessageRetentionPeriod)) * time.Second
	limit := now.Add(-retention)

	for top := c.visible.peek(); top != nil && !top.msg.sent.After(limit); top = c.visible.peek() {
		c.remove(top)
	}
}

// rearm sets the timer for the earliest deadline, or stops it when there is
// none.
func (c *standardContents) rearm(now time.Time) {
	top := c.deadlines.peek()
	if top == nil {
		if c.armed {
			c.timer.Stop()
			c.armed = false
		}

		return
	}

	if c.armed && c.armedFor.Equal(top.deadline) {
		return
	}

	wait := max(top.deadline.Sub(now), 0)

	if c.timer == nil {
		c.timer = time.AfterFunc(wait, c.fire)
	} else {
		c.timer.Reset(wait)
	}

	c.armed, c.armedFor = true, top.deadline
}

// fire is the timer callback. It takes the queue's lock like any caller.
func (c *standardContents) fire() {
	c.q.mu.Lock()
	defer c.q.mu.Unlock()

	if c.closed {
		return
	}

	// The timer that called this is spent: rearm sets it again if a deadline is
	// left.
	c.armed = false
	now := time.Now()
	c.refresh(now)
	c.rearm(now)
}

// timerArmed reports whether a timer is set. Tests use it to check that an idle
// queue holds none.
func (c *standardContents) timerArmed() bool { return c.armed }

// newReceiptHandle makes a receipt handle the way LocalStack does: the base64
// of a random id, the queue's ARN, the message id and the time of the receive.
// The ARN lets a queue refuse the handle of another one.
func newReceiptHandle(queueARN, messageID string, now time.Time) string {
	received := strconv.FormatFloat(float64(now.UnixMicro())/1e6, 'f', 6, 64)

	return base64.StdEncoding.EncodeToString([]byte(newRequestID() + " " + queueARN + " " + messageID + " " + received))
}
