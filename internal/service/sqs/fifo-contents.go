package sqs

import (
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kylecannon/gostack/internal/awserr"
)

// scopeMessageGroup is the DeduplicationScope that keeps an id per message
// group. Any other value deduplicates over the whole queue.
const scopeMessageGroup = "messageGroup"

// sequenceShift puts the start of the sequence numbers at the current second in
// the high bits, as LocalStack does, so numbers of two runs do not overlap.
const sequenceShift = 33

var (
	sequenceBase    = sync.OnceValue(sequenceStart)
	sequenceCounter atomic.Uint64
)

func sequenceStart() uint64 {
	return uint64(time.Now().Unix()) << sequenceShift //nolint:gosec // G115: seconds are positive.
}

// nextSequence returns the SequenceNumber of the next message of any FIFO queue.
// The numbers of one queue only ever grow.
func nextSequence() string {
	return strconv.FormatUint(sequenceBase()+sequenceCounter.Add(1), 10)
}

// raiseSequence makes the next SequenceNumber larger than the one given, which a
// restored message holds. The start of the numbers is the clock, so only a clock
// that went back since the snapshot can make this do anything.
func raiseSequence(restored string) {
	n, err := strconv.ParseUint(restored, 10, 64)
	if err != nil || n < sequenceBase() {
		return
	}

	want := n - sequenceBase()

	for {
		current := sequenceCounter.Load()
		if current >= want || sequenceCounter.CompareAndSwap(current, want) {
			return
		}
	}
}

// fifoContents is the Contents of a FIFO queue.
//
// Messages are in groups. A receive takes from the group that became ready first
// and from the next ones while it wants more, and locks each group it took from
// until a message of it is visible again or the last one in flight is deleted
// (see group). Delayed messages, the head of each group's in-flight messages and
// the records of deduplication ids share one heap by deadline and one timer, as
// the standard contents' delayed and in-flight messages do, so a queue with
// nothing pending costs no wakeups.
type fifoContents struct {
	q        *Queue
	settings func() Settings
	// scope is the DeduplicationScope the queue was created with. As in
	// LocalStack, changing the attribute afterwards does not change it.
	scope string

	groups    map[string]*group
	ready     readyList
	deadlines entryHeap
	receipts  map[string]*entry
	dedup     map[string]*entry
	seq       uint64
	visible   int
	delayed   int
	inflight  int
	// expired counts the messages retention has dropped, for Receive to tell
	// whether its own refresh dropped some.
	expired uint64
	redrive redriveCache

	wake  waker
	timer deadlineTimer
}

var _ Contents = (*fifoContents)(nil)

func newFIFOContents(q *Queue, settings func() Settings) *fifoContents {
	return &fifoContents{
		q:        q,
		settings: settings,
		scope:    q.attrs[attrDeduplicationScope],
		groups:   make(map[string]*group),
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
		dedup:    make(map[string]*entry),
	}
}

func (c *fifoContents) Counts() Counts {
	c.refresh(time.Now())

	return Counts{Visible: c.visible, NotVisible: c.inflight, Delayed: c.delayed}
}

func (c *fifoContents) Clear() {
	c.groups = make(map[string]*group)
	c.ready.reset()
	c.deadlines.reset()
	clear(c.receipts)
	clear(c.dedup)
	c.visible, c.delayed, c.inflight = 0, 0, 0
	c.rearm(time.Now())
}

func (c *fifoContents) Close() {
	c.Clear()
	c.wake.shutdown()
}

func (c *fifoContents) Wake() <-chan struct{} { return c.wake.channel() }

func (c *fifoContents) Put(m *message, delay time.Duration) {
	key := c.dedupKey(m)

	if original, ok := c.dedup[key]; ok && m.sent.Before(original.fifo.expires) {
		// A duplicate is not sent again, and its sender is told about the first.
		m.id, m.sequence = original.fifo.original, original.fifo.sequence

		return
	}

	m.sequence = nextSequence()
	m.system[sysSequenceNumber] = m.sequence
	m.system[sysMessageDeduplicationID] = m.dedupID
	c.remember(key, m)

	c.seq++
	g := c.group(m.groupID)
	e := &entry{msg: m, seq: c.seq, visibleAt: -1, deadlineAt: -1, fifo: &fifoState{group: g}}

	if delay > 0 {
		e.state = stateDelayed
		e.deadline = m.sent.Add(delay)
		c.deadlines.push(e)
		c.delayed++
		g.delayed++
	} else {
		c.makeVisible(e)
	}

	c.rearm(m.sent)
}

func (c *fifoContents) Receive(maxMessages int, visibility time.Duration) receiveResult {
	now := time.Now()
	out := receiveResult{expired: c.refreshed(now)}

	var (
		taken   []*entry
		touched []*group
	)

	for len(taken) < maxMessages {
		g := c.nextGroup()
		if g == nil {
			break
		}

		g.locked = true
		touched = append(touched, g)
		taken = c.takeFrom(g, maxMessages, taken, &out)
	}

	out.taken = make([]delivery, 0, len(taken))

	for _, e := range taken {
		out.taken = append(out.taken, c.deliver(e, now))

		if visibility <= 0 {
			c.makeVisible(e)
		} else {
			c.enterFlight(e, now.Add(visibility))
		}
	}

	// A group whose messages all left, as dead letters, is not in flight.
	for _, g := range touched {
		c.reopenIfIdle(g)
	}

	c.rearm(now)

	return out
}

func (c *fifoContents) Delete(handle string) *awserr.Error {
	e, ok := c.receipts[handle]
	if !ok {
		return nil
	}

	now := time.Now()
	c.refresh(now)

	// A message that is visible again, or only held back behind an earlier one,
	// has no handle that is good any more.
	if e.state != stateInflight || !e.deadline.After(now) {
		return invalidParameterValue("Value " + handle + " for parameter ReceiptHandle is invalid. " +
			"Reason: The receipt handle has expired.")
	}

	g := e.fifo.group
	c.leaveFlight(e)
	c.forget(e)
	c.reopenIfIdle(g)
	c.rearm(now)

	return nil
}

func (c *fifoContents) ChangeVisibility(handle string, visibility time.Duration) bool {
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
		c.leaveFlight(e)
		c.makeVisible(e)
	} else {
		e.deadline = now.Add(visibility)

		if e.deadlineAt >= 0 {
			c.deadlines.fix(e)
		}
	}

	c.rearm(now)

	return true
}

// dedupKey is what a message is deduplicated on: its id, and with the
// messageGroup scope its group too.
func (c *fifoContents) dedupKey(m *message) string {
	if c.scope == scopeMessageGroup {
		return m.groupID + "\x00" + m.dedupID
	}

	return m.dedupID
}

// remember records the deduplication id of m for the window. The record leaves
// the heap with the first whole second after the window, which keeps a queue
// that sends a lot from waking once per message; the window itself ends exactly.
func (c *fifoContents) remember(key string, m *message) {
	record := &entry{
		state:      stateDedup,
		deadline:   ceilSecond(m.sent.Add(dedupWindow)),
		visibleAt:  -1,
		deadlineAt: -1,
		fifo: &fifoState{
			dedupKey: key, original: m.id, sequence: m.sequence, expires: m.sent.Add(dedupWindow),
		},
	}

	c.dedup[key] = record
	c.deadlines.push(record)
}

// group returns the group with the id, making it when there is none.
func (c *fifoContents) group(id string) *group {
	g, ok := c.groups[id]
	if !ok {
		g = newGroup(id)
		c.groups[id] = g
	}

	return g
}

// nextGroup takes the group that became ready first out of the ready list, or
// returns nil when there is none.
func (c *fifoContents) nextGroup() *group {
	for c.ready.len() > 0 {
		g := c.ready.pop()
		g.queued = false

		if g.visible.len() > 0 {
			return g
		}

		c.forgetIfIdle(g)
	}

	return nil
}

// takeFrom takes visible messages of g into taken until there are max of them,
// and sends the ones past the redrive limit to out.dead instead.
func (c *fifoContents) takeFrom(g *group, maxMessages int, taken []*entry, out *receiveResult) []*entry {
	policy, redriven := c.redrive.of(c.q)
	redriven = redriven && policy.maxReceives > 0

	for len(taken) < maxMessages && g.visible.len() > 0 {
		e := g.visible.pop()
		c.visible--
		e.receiveCount++

		if redriven && e.receiveCount > policy.maxReceives {
			c.forget(e)
			out.dead = append(out.dead, deadLetter{
				msg: e.msg, target: policy.target, received: e.msg.receiveBase + e.receiveCount - 1,
			})

			continue
		}

		taken = append(taken, e)
	}

	return taken
}

// deliver gives a receive of e its receipt handle.
func (c *fifoContents) deliver(e *entry, now time.Time) delivery {
	if e.firstReceived.IsZero() {
		e.firstReceived = now
	}

	handle := newReceiptHandle(c.q.arn, e.msg.id, now)
	e.handles = append(e.handles, handle)
	c.receipts[handle] = e

	return delivery{
		msg: e.msg, handle: handle, receiveCount: e.msg.receiveBase + e.receiveCount, firstReceived: e.firstReceived,
	}
}

// makeVisible puts e in its group, where a receive finds it. A message that was
// received before opens the group, a new one does not open a locked group.
func (c *fifoContents) makeVisible(e *entry) {
	g := e.fifo.group
	e.state = stateVisible
	e.deadline = time.Time{}
	g.visible.push(e)
	c.visible++

	if e.receiveCount > 0 {
		g.locked = false
	}

	c.makeReady(g)
}

// makeReady puts a group that has messages and is not locked in the ready list,
// and wakes the long polls.
func (c *fifoContents) makeReady(g *group) {
	if g.locked || g.queued || g.visible.len() == 0 {
		return
	}

	g.queued = true
	c.ready.push(g)
	c.wake.notify()
}

// reopenIfIdle opens a locked group that has no message in flight any more.
func (c *fifoContents) reopenIfIdle(g *group) {
	if g.locked && g.inflight == 0 {
		g.locked = false
		c.makeReady(g)
	}

	c.forgetIfIdle(g)
}

// forgetIfIdle drops a group that holds nothing.
func (c *fifoContents) forgetIfIdle(g *group) {
	if g.idle() && c.groups[g.id] == g {
		delete(c.groups, g.id)
	}
}

// enterFlight puts e at the end of its group's in-flight messages.
func (c *fifoContents) enterFlight(e *entry, deadline time.Time) {
	g := e.fifo.group
	e.state = stateInflight
	e.deadline = deadline
	e.fifo.prev, e.fifo.next = g.tail, nil

	if g.tail != nil {
		g.tail.fifo.next = e
	} else {
		g.head = e
		c.deadlines.push(e)
	}

	g.tail = e
	g.inflight++
	c.inflight++
}

// leaveFlight takes e out of its group's in-flight messages. When e was the head
// its successor becomes the head and enters the deadline heap.
func (c *fifoContents) leaveFlight(e *entry) {
	g := e.fifo.group
	wasHead := g.head == e

	if e.deadlineAt >= 0 {
		c.deadlines.remove(e)
	}

	if e.fifo.prev != nil {
		e.fifo.prev.fifo.next = e.fifo.next
	} else {
		g.head = e.fifo.next
	}

	if e.fifo.next != nil {
		e.fifo.next.fifo.prev = e.fifo.prev
	} else {
		g.tail = e.fifo.prev
	}

	e.fifo.prev, e.fifo.next = nil, nil
	g.inflight--
	c.inflight--

	if wasHead && g.head != nil {
		c.deadlines.push(g.head)
	}
}

// forget drops every receipt handle that names e.
func (c *fifoContents) forget(e *entry) {
	for _, handle := range e.handles {
		delete(c.receipts, handle)
	}

	e.handles = nil
}

// refresh brings the queue up to now: the delayed messages whose delay is over
// and the in-flight ones whose timeout is over become visible, the records of
// deduplication ids past their window go, and the visible messages past
// retention are dropped.
func (c *fifoContents) refresh(now time.Time) {
	for top := c.deadlines.peek(); top != nil && !top.deadline.After(now); top = c.deadlines.peek() {
		c.deadlines.remove(top)

		switch top.state {
		case stateDedup:
			if c.dedup[top.fifo.dedupKey] == top {
				delete(c.dedup, top.fifo.dedupKey)
			}
		case stateDelayed:
			c.delayed--
			top.fifo.group.delayed--
			c.makeVisible(top)
		default:
			c.leaveFlight(top)
			c.makeVisible(top)
		}
	}

	c.sweepRetention(now)
}

// refreshed is refresh, and reports whether retention dropped messages.
func (c *fifoContents) refreshed(now time.Time) bool {
	before := c.expired

	c.refresh(now)

	return c.expired != before
}

// sweepRetention drops the visible messages that are older than the queue's
// MessageRetentionPeriod, when the setting is on. A message in flight or still
// delayed stays until it is visible, as in LocalStack.
func (c *fifoContents) sweepRetention(now time.Time) {
	if !c.settings().EnableMessageRetentionPeriod {
		return
	}

	retention := time.Duration(c.q.intAttr(attrMessageRetentionPeriod)) * time.Second
	limit := now.Add(-retention)

	for _, g := range c.groups {
		for top := g.visible.peek(); top != nil && !top.msg.sent.After(limit); top = g.visible.peek() {
			g.visible.remove(top)
			c.visible--
			c.forget(top)
			c.expired++
		}

		c.forgetIfIdle(g)
	}
}

// rearm sets the timer for the earliest deadline, or stops it when there is
// none.
func (c *fifoContents) rearm(now time.Time) {
	c.timer.arm(c.deadlines.peek(), now, c.fire)
}

// fire is the timer callback. It takes the queue's lock like any caller.
func (c *fifoContents) fire() {
	c.q.mu.Lock()
	defer c.q.mu.Unlock()

	if c.wake.closed {
		return
	}

	c.timer.spent()
	now := time.Now()
	c.refresh(now)
	c.rearm(now)
}
