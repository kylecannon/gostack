package sns

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kylecannon/gostack/internal/inproc"
)

const (
	// laneDepth is how many deliveries one destination's lane holds, the one
	// being sent included. Each lane is bounded, so memory is, and a stuck
	// destination fills only its own lane.
	laneDepth = 256

	// roomWait is how long a publish waits for room in the full lanes of its
	// destinations, all of them together. It is well under an SDK's read
	// timeout, so the publish is answered before its client gives up.
	roomWait = 10 * time.Second

	// logInterval is how often the failed deliveries of one destination are
	// logged, as one line with their count.
	logInterval = 5 * time.Second
)

// The reasons a delivery is dropped or dead-lettered, as they are logged.
const (
	reasonNoRoom     = "no room in the destination's lane before the deadline"
	reasonDLQNoRoom  = "no room in the dead-letter queue's lane"
	reasonClosing    = "SNS is shutting down"
	reasonCut        = "the in-process client stopped before the delivery was made"
	reasonDLQRefused = "the dead-letter queue did not take the message"
)

// lanes deliver SNS's messages, one lane per destination: the deliveries to
// one queue, or one endpoint, wait in its lane and are sent one at a time, in
// the order they were queued, so a destination's deliveries keep the order of
// their publishes, and a slow destination holds up no other.
//
// A lane has no goroutine of its own. Its head is with the transport, an SQS
// SendMessage queued on the in-process client, and the transport's answer
// sends the next, so a lane with work always has a call in the client, which
// is what a shutdown's drain of the client waits for, and an empty lane is
// gone.
//
// Every delivery that enters a lane leaves it once, under mu: sent, failed
// (to its dead-letter queue or dropped), or cut. queued counts the ones in a
// lane, and dropped the ones lost.
type lanes struct {
	client *inproc.Client
	// depth and wait are laneDepth and roomWait; tests shorten them.
	depth int
	wait  time.Duration

	mu    sync.Mutex
	byKey map[string]*lane
	// parked are lanes whose head the client refused because its queue was
	// full; the next answer starts them again.
	parked []*lane
	// inflight counts the heads handed to the client and not yet answered.
	inflight int
	queued   int
	// idle, when not nil, is closed when queued reaches zero.
	idle chan struct{}
	// unbounded is set when the shutdown begins: no delivery waits any more.
	// closing is set when Close begins, closed when it stops waiting.
	unbounded, closing, closed bool

	dropped atomic.Int64
	log     dropLog
}

// lane is the deliveries to one destination. jobs[:depth] are admitted; a
// publisher's deliveries past them wait for room, in order.
type lane struct {
	key  string
	jobs []*job
	// sending is set while jobs[0] is with the transport.
	sending bool
	parked  bool
	// held is a dead-letter delivery of this lane's that waits for room in
	// its queue's lane: this lane sends nothing until it is placed.
	held *job
	// done is the transport's answer to this lane's head.
	done func(inproc.Response, error)
}

// job is one delivery.
type job struct {
	lane *lane
	call inproc.Call
	// dlq is the call that puts the message on its subscription's dead-letter
	// queue, nil without one.
	dlq *inproc.Call
	// dead marks a dead-letter delivery: one that fails is dropped, never sent
	// to another dead-letter queue.
	dead bool
	// waiting is set while the job is in its lane past the admitted ones.
	waiting bool
	// admit, for a publisher's waiting delivery, is closed when the delivery
	// is admitted or leaves its lane.
	admit chan struct{}
	// origin is the lane a waiting dead-letter delivery holds up, and expire
	// the timer that ends its wait.
	origin *lane
	expire *time.Timer
}

// mayWait is what j does when its destination's lane is full: it waits for
// room, up to its deadline, when something can wait for it, and is dropped
// otherwise. A publisher's delivery has its publisher; a dead-letter delivery
// has the lane it failed in (origin), and has none when it failed at its
// publisher's deadline, which has passed. The ruling (SQ-288, orchestrator, provisional; the user
// has not ruled on waiting versus dropping) is that a delivery to a healthy
// destination is never dropped and memory stays bounded, so the bound is paid
// in time: a publisher waits, and the lane a dead-letter delivery comes from
// pauses. At the deadline the delivery fails, to its dead-letter queue or
// dropped, and Publish still answers 200. Returning false here drops every
// delivery that finds its lane full at once, which reverses the ruling.
func mayWait(j *job) bool {
	return !j.dead || j.origin != nil
}

// add puts j at the tail of the lane of its destination. It is the one place a
// delivery is refused: a publisher's once Close has begun, any once Close has
// stopped waiting, and one that finds its lane full and may not wait. A
// refused delivery is counted and logged as dropped. add reports whether j is
// in the lane, admitted or waiting.
func (ls *lanes) add(j *job) bool {
	if ls.closed || !j.dead && ls.closing {
		ls.drop(j, reasonClosing)

		return false
	}

	l := ls.byKey[j.call.Lane]

	full := !ls.unbounded && l != nil && len(l.jobs) >= ls.depth
	if full && !mayWait(j) {
		reason := reasonNoRoom
		if j.dead {
			reason = reasonDLQNoRoom
		}

		ls.drop(j, reason)

		return false
	}

	if l == nil {
		l = &lane{key: j.call.Lane}
		l.done = func(resp inproc.Response, err error) { ls.answered(l, resp, err) }

		if ls.byKey == nil {
			ls.byKey = make(map[string]*lane)
		}

		ls.byKey[l.key] = l
	}

	j.lane = l
	j.waiting = full

	if full && !j.dead {
		j.admit = make(chan struct{})
	}

	l.jobs = append(l.jobs, j)

	if ls.queued == 0 {
		ls.idle = make(chan struct{})
	}

	ls.queued++

	return true
}

// dispatch starts the lanes of a publish's deliveries and waits, with no lock
// held, for the ones that found their lane full to be admitted, until the
// publish's deadline, one for all of them. A delivery still waiting then
// fails. The order was fixed when the deliveries were added.
func (ls *lanes) dispatch(jobs []*job) {
	if len(jobs) == 0 {
		return
	}

	var waiting []*job

	ls.mu.Lock()

	for _, j := range jobs {
		if j.lane == nil {
			continue
		}

		ls.start(j.lane)

		if j.waiting {
			waiting = append(waiting, j)
		}
	}

	ls.mu.Unlock()

	if len(waiting) == 0 {
		return
	}

	deadline := time.NewTimer(ls.wait)
	defer deadline.Stop()

	for i, j := range waiting {
		select {
		case <-j.admit:
		case <-deadline.C:
			ls.mu.Lock()

			for _, late := range waiting[i:] {
				ls.expire(late)
			}

			ls.mu.Unlock()

			return
		}
	}
}

// expire fails a publisher's delivery still waiting at its deadline. Its
// dead-letter delivery may not wait: the deadline has passed.
func (ls *lanes) expire(j *job) {
	if !j.waiting || j.lane == nil {
		return
	}

	l := j.lane
	ls.remove(j)
	ls.fail(j, nil, reasonNoRoom)
	ls.tidy(l)
}

// start hands l's head to the transport when nothing holds the lane up. When
// the client refuses it with a call of SNS's still in it, its queue is full,
// and the lane is parked until an answer frees room; with none, the client
// was stopped, and every delivery in the lane is cut.
func (ls *lanes) start(l *lane) {
	if l.sending || l.parked || l.held != nil || len(l.jobs) == 0 {
		ls.tidy(l)

		return
	}

	l.sending = true
	ls.inflight++

	if ls.client.Enqueue(l.jobs[0].call, l.done) {
		return
	}

	l.sending = false
	ls.inflight--

	if ls.inflight > 0 {
		l.parked = true
		ls.parked = append(ls.parked, l)

		return
	}

	for len(l.jobs) > 0 {
		j := l.jobs[0]
		ls.remove(j)
		ls.drop(j, reasonCut)
	}

	ls.tidy(l)
}

// answered is the transport's answer to l's head, on the client's worker. It
// takes the head out of the lane, fails it when it was not delivered, and
// sends the next.
func (ls *lanes) answered(l *lane, resp inproc.Response, err error) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	ls.inflight--
	l.sending = false

	if len(l.jobs) == 0 {
		// Close cut it already.
		return
	}

	j := l.jobs[0]
	ls.remove(j)

	switch {
	case err == nil && resp.OK():
	case ls.closed:
		ls.drop(j, reasonCut)
	default:
		reason := "SQS answered " + strconv.Itoa(resp.Status)
		if err != nil {
			reason = err.Error()
		}

		if j.dead {
			reason = reasonDLQRefused + ": " + reason
		}

		ls.fail(j, l, reason)
	}

	if ls.closed {
		ls.tidy(l)

		return
	}

	ls.start(l)
	ls.unpark()
}

// unpark starts the parked lanes again, until the client refuses one.
func (ls *lanes) unpark() {
	for len(ls.parked) > 0 {
		l := ls.parked[0]
		ls.parked[0] = nil
		ls.parked = ls.parked[1:]
		l.parked = false

		ls.start(l)

		if l.parked {
			return
		}
	}
}

// fail is a delivery that was not made: it goes to its dead-letter queue when
// it has one, and is dropped otherwise. origin is the lane of a delivery that
// failed on its transport: when the dead-letter queue's lane is full, origin
// pauses until it has room, up to the deadline. A delivery that failed at a
// publisher's deadline has no origin and does not wait.
func (ls *lanes) fail(j *job, origin *lane, reason string) {
	if j.dlq == nil || j.dead {
		ls.drop(j, reason)

		return
	}

	dead := &job{call: *j.dlq, dead: true}

	// A lane cannot wait for room in itself.
	if origin != nil && origin.key != dead.call.Lane {
		dead.origin = origin
	}

	if !ls.add(dead) {
		return
	}

	ls.log.deadLettered(j.call.Lane, reason)

	if dead.waiting {
		origin.held = dead
		dead.expire = time.AfterFunc(ls.wait, func() { ls.expireHeld(dead) })
	} else {
		dead.origin = nil
	}

	ls.start(dead.lane)
}

// expireHeld drops a dead-letter delivery that found no room by its deadline,
// and lets the lane it held up go on.
func (ls *lanes) expireHeld(dead *job) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	if !dead.waiting || dead.lane == nil {
		return
	}

	l := dead.lane
	ls.remove(dead)
	ls.drop(dead, reasonDLQNoRoom)
	ls.tidy(l)
}

// admitJob admits a waiting delivery: its publisher stops waiting, or the lane
// it held up goes on.
func (ls *lanes) admitJob(j *job) {
	if !j.waiting {
		return
	}

	j.waiting = false

	if j.admit != nil {
		close(j.admit)
	}

	ls.release(j)
}

// release lets the lane a dead-letter delivery held up go on.
func (ls *lanes) release(j *job) {
	origin := j.origin
	if origin == nil {
		return
	}

	j.origin = nil
	j.expire.Stop()
	origin.held = nil

	if !ls.closed {
		ls.start(origin)
	}
}

// remove takes j out of its lane, which it leaves for good. When it was
// admitted, the first delivery waiting behind the admitted ones takes its
// place.
func (ls *lanes) remove(j *job) {
	l := j.lane
	i := slices.Index(l.jobs, j)
	admitted := i < ls.depth

	if i == 0 {
		l.jobs[0] = nil
		l.jobs = l.jobs[1:]
	} else {
		l.jobs = slices.Delete(l.jobs, i, i+1)
	}

	j.lane = nil

	if j.waiting {
		j.waiting = false

		if j.admit != nil {
			close(j.admit)
		}

		ls.release(j)
	}

	if admitted && len(l.jobs) >= ls.depth {
		ls.admitJob(l.jobs[ls.depth-1])
	}

	ls.queued--
	if ls.queued == 0 {
		close(ls.idle)
		ls.idle = nil
	}
}

// tidy forgets a lane with nothing in it.
func (ls *lanes) tidy(l *lane) {
	if len(l.jobs) == 0 && !l.sending && !l.parked && l.held == nil && ls.byKey[l.key] == l {
		delete(ls.byKey, l.key)
	}
}

// drop counts and logs a delivery that is lost.
func (ls *lanes) drop(j *job, reason string) {
	ls.dropped.Add(1)
	ls.log.dropped(j.call.Lane, reason)
}

// unbound admits every waiting delivery and lets no delivery wait from now
// on: the shutdown has begun, and its drain has a deadline of its own.
func (ls *lanes) unbound() {
	ls.unbounded = true

	for _, l := range ls.byKey {
		for _, j := range l.jobs {
			ls.admitJob(j)
		}
	}
}

// awaitIdle returns when no delivery is in a lane, or ctx ends.
func (ls *lanes) awaitIdle(ctx context.Context) {
	ls.mu.Lock()
	idle := ls.idle
	ls.mu.Unlock()

	if idle == nil {
		return
	}

	select {
	case <-idle:
	case <-ctx.Done():
	}
}

// Close stops delivery. Every delivery still in a lane is accounted for once:
// one that cannot be made any more is counted in Dropped and logged.
//
// From the moment Close begins, a publish's deliveries are refused, and no
// delivery waits for room. Close then waits, until ctx ends, for the
// deliveries still in a lane, so a publish that was acknowledged reaches its
// queue on a clean stop. It does not wait when the client was stopped
// already, as a shutdown's drain of the in-process calls stops it at its
// deadline: a head that was then queued in the client is never answered, and
// is cut here instead. Last, Close stops the client and cuts what is left.
func (p *Provider) Close(ctx context.Context) {
	ls := &p.lanes

	ls.mu.Lock()
	ls.closing = true
	ls.unbound()
	// A head with SNS while the client is idle is one the client dropped
	// without answering: the client answers each head before its worker
	// leaves, and SNS hands it heads under mu.
	stopped := ls.inflight > 0 && p.client.Idle()
	ls.mu.Unlock()

	if !stopped {
		ls.awaitIdle(ctx)
	}

	ls.mu.Lock()
	ls.closed = true
	ls.mu.Unlock()

	// Close answers the heads it cancels, which answered drops, and drops the
	// ones still queued without an answer.
	p.client.Close()

	ls.mu.Lock()

	for _, l := range ls.byKey {
		for len(l.jobs) > 0 {
			j := l.jobs[0]
			ls.remove(j)
			ls.drop(j, reasonCut)
		}

		l.sending, l.parked = false, false
		ls.tidy(l)
	}

	ls.parked = nil
	ls.inflight = 0
	ls.mu.Unlock()

	ls.log.flush()
}

// shutdown is the gateway's word that it has begun to shut down: no delivery
// waits for room from now on, so a publisher waiting is answered, and its
// deliveries are made in the drain.
func (p *Provider) shutdown() {
	p.lanes.mu.Lock()
	p.lanes.unbound()
	p.lanes.mu.Unlock()
}

// Dropped is the number of deliveries lost: not made, and not taken by a
// dead-letter queue.
func (p *Provider) Dropped() int64 {
	return p.lanes.dropped.Load()
}

// dropLog logs failed deliveries at most once per destination per
// logInterval, with their count. A timer runs only while a count waits to be
// logged.
type dropLog struct {
	mu     sync.Mutex
	counts map[string]*dropCount
	timer  *time.Timer
}

// dropCount is what a destination lost since its last line.
type dropCount struct {
	dropped, deadLettered int
	reason                string
}

// dropped counts a delivery to destination that was lost.
func (d *dropLog) dropped(destination, reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.count(destination, reason).dropped++
}

// deadLettered counts a delivery to destination that went to its
// subscription's dead-letter queue instead.
func (d *dropLog) deadLettered(destination, reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.count(destination, reason).deadLettered++
}

// count is destination's count, under mu, with reason as its latest. It arms
// the timer that logs it.
func (d *dropLog) count(destination, reason string) *dropCount {
	if d.counts == nil {
		d.counts = make(map[string]*dropCount)
	}

	c := d.counts[destination]
	if c == nil {
		c = &dropCount{}
		d.counts[destination] = c
	}

	c.reason = reason

	if d.timer == nil {
		d.timer = time.AfterFunc(logInterval, d.flush)
	}

	return c
}

// flush logs the counts waiting.
func (d *dropLog) flush() {
	d.mu.Lock()
	counts := d.counts
	d.counts = nil

	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}

	d.mu.Unlock()

	for _, destination := range slices.Sorted(maps.Keys(counts)) {
		c := counts[destination]
		slog.Warn("SNS deliveries failed", "destination", destination, "dropped", c.dropped,
			"dead_lettered", c.deadLettered, "last_reason", c.reason)
	}
}
