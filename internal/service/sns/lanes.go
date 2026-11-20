package sns

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
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
// sends the next; an empty lane is gone. A held or parked lane has work and no
// call in the client, so the lanes hold the hub's pending count while any
// delivery is in them (unhold), which is what a shutdown's drain of the
// client waits for.
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
	// unhold, while queued is above zero, lets go of the hold the deliveries
	// have on the in-process hub's pending count (inproc.Client.Hold), so a
	// shutdown's drain of the calls waits for a delivery that is not a call
	// in the client: one waiting in its lane, in a parked or a held lane, or
	// added and not yet started.
	unhold func()
	// unbounded is set when the shutdown begins: no delivery waits any more.
	// closed is set when Close begins.
	unbounded, closed bool

	dropped atomic.Int64
	log     dropLog
}

// lane is the deliveries to one destination. jobs[:admitted] are admitted, up
// to depth, and past it only for a dead-letter delivery that may not wait (see
// add); the deliveries after them wait for room, in order.
type lane struct {
	key      string
	jobs     []*job
	admitted int
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
	// to another dead-letter queue. handoff marks one made at its publisher's
	// deadline.
	dead, handoff bool
	// expired marks a publisher's delivery to a FIFO queue that was still
	// waiting at its publisher's deadline and keeps its place in the lane:
	// when it reaches the head it fails, to its dead-letter queue, after the
	// deliveries ahead of it, so a message group keeps its order there.
	expired bool
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
// publisher's deadline, which has passed. A dead-letter delivery never waits
// for a lane that is itself held up by a dead-letter delivery of its own: that
// lane may be waiting, directly or not, for the one this delivery holds up, so
// two queues that are each other's dead-letter queue would stall each other
// to the deadline. The ruling (SQ-288, orchestrator, provisional; the user
// has not ruled on waiting versus dropping) is that a delivery to a healthy
// destination is never dropped and memory stays bounded, so the bound is paid
// in time: a publisher waits, and the lane a dead-letter delivery comes from
// pauses. At the deadline the delivery fails, to its dead-letter queue or
// dropped, and Publish still answers 200. Returning false here fails every
// delivery that finds its lane full at once, to its dead-letter queue or
// dropped, as the withdrawn contract did: that reverses the ruling.
func mayWait(j *job, l *lane) bool {
	return !j.dead || (j.origin != nil && l.held == nil)
}

// overflow reports whether dead, a dead-letter delivery that found l full and
// may not wait, is admitted past l's depth: up to a ceiling of twice the
// depth, and for a hand-off at a publisher's deadline only when nothing waits
// in l, since the delivery it replaces already held its memory while it
// waited. One that is not is dropped.
func (ls *lanes) overflow(dead *job, l *lane) bool {
	return l.admitted < 2*ls.depth && (!dead.handoff || len(l.jobs) == l.admitted)
}

// add puts j in the lane of its destination: admitted when the lane has room,
// after the deliveries waiting in it otherwise. It is the one place a
// delivery is refused: any once Close has begun, and one that finds its lane
// full and may not wait, unless it is a dead-letter delivery admitted past the
// depth (overflow), ahead of the deliveries waiting. A delivery refused at
// Close is dropped; one refused for room fails, to its dead-letter queue when
// it has one. Each is counted and logged. add reports whether j is in the
// lane, admitted or waiting.
func (ls *lanes) add(j *job) bool {
	if ls.closed {
		ls.drop(j, reasonClosing)

		return false
	}

	l := ls.byKey[j.call.Lane]
	room := ls.unbounded || l == nil || (l.admitted < ls.depth && len(l.jobs) == l.admitted)
	at := -1 // the tail

	switch {
	case room:
	case mayWait(j, l):
		j.waiting = true

		// A dead-letter delivery waits ahead of the publishers' deliveries,
		// behind the dead-letter deliveries waiting already: the lane it holds
		// up sends nothing meanwhile, and a publisher has its own deadline.
		if j.dead {
			at = l.admitted
			for at < len(l.jobs) && l.jobs[at].dead {
				at++
			}
		}
	case j.dead && ls.overflow(j, l):
		at = l.admitted
	case j.dead:
		ls.drop(j, reasonDLQNoRoom)

		return false
	default:
		ls.fail(j, nil, reasonNoRoom)

		return false
	}

	ls.place(j, l, at)

	return true
}

// place puts j in l, or in a new lane when l is nil: at index at when it is
// not negative, and at the tail otherwise; admitted unless it waits. j is
// counted in queued.
func (ls *lanes) place(j *job, l *lane, at int) {
	if l == nil {
		l = &lane{key: j.call.Lane}
		l.done = func(resp inproc.Response, err error) { ls.answered(l, resp, err) }

		if ls.byKey == nil {
			ls.byKey = make(map[string]*lane)
		}

		ls.byKey[l.key] = l
	}

	j.lane = l

	switch {
	case j.waiting && j.dead:
		l.jobs = slices.Insert(l.jobs, at, j)
	case j.waiting:
		j.admit = make(chan struct{})
		l.jobs = append(l.jobs, j)
	case at >= 0:
		l.jobs = slices.Insert(l.jobs, at, j)
		l.admitted++
	default:
		l.jobs = append(l.jobs, j)
		l.admitted++
	}

	if ls.queued == 0 {
		ls.idle = make(chan struct{})
		ls.unhold = ls.client.Hold()
	}

	ls.queued++
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
	wait := ls.wait

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

	deadline := time.NewTimer(wait)
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
// dead-letter delivery may not wait: the deadline has passed. A delivery to a
// FIFO queue with a dead-letter queue keeps its place instead, up to twice
// the lane's depth, and fails when it reaches the head (start), so its message
// group reaches the dead-letter queue in order.
func (ls *lanes) expire(j *job) {
	if !j.waiting || j.lane == nil || j.expired {
		return
	}

	l := j.lane

	if j.dlq != nil && strings.HasSuffix(l.key, fifoSuffix) && slices.Index(l.jobs, j) < 2*ls.depth {
		j.expired = true

		return
	}

	ls.queued++ // see settle
	ls.remove(j)
	ls.fail(j, nil, reasonNoRoom)
	ls.settle()
	ls.tidy(l)
}

// start hands l's head to the transport when nothing holds the lane up. An
// expired head fails instead (see expire), and the next is started. When the
// client refuses the head with a call of SNS's still in it, its queue is
// full, and the lane is parked until an answer frees room; with none, the
// client was stopped, and every delivery in the lane is cut.
func (ls *lanes) start(l *lane) {
	for !ls.closed && !l.sending && !l.parked && l.held == nil && len(l.jobs) > 0 && l.jobs[0].expired {
		j := l.jobs[0]
		ls.queued++ // see settle
		ls.remove(j)
		ls.fail(j, l, reasonNoRoom)
		ls.settle()
	}

	if ls.closed || l.sending || l.parked || l.held != nil || len(l.jobs) == 0 {
		ls.tidy(l)

		return
	}

	l.sending = true
	ls.inflight++

	if ls.send(l) {
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

// send hands l's head to its transport, which answers through l.done, on a
// goroutine of its own, and never blocks; it reports whether the transport
// took it. It is the one place a lane meets a transport: an SQS SendMessage
// queued on the in-process client, which counts in inflight.
func (ls *lanes) send(l *lane) bool {
	return ls.client.Enqueue(l.jobs[0].call, l.done)
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
	ls.queued++ // see settle
	ls.remove(j)

	defer ls.settle()

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

	// The parked lanes go first, so a lane with more to send does not take
	// the room its answer freed from them again and again; it is parked
	// behind them when none is left.
	ls.unpark()
	ls.start(l)
}

// unpark starts the parked lanes again, in the order they were parked, until
// the client refuses one, which keeps its place at the front.
func (ls *lanes) unpark() {
	for len(ls.parked) > 0 {
		l := ls.parked[0]
		ls.parked[0] = nil
		ls.parked = ls.parked[1:]
		l.parked = false

		ls.start(l)

		if l.parked {
			// start parked it again, at the back.
			ls.parked = slices.Insert(ls.parked[:len(ls.parked)-1], 0, l)

			return
		}
	}
}

// fail is a delivery that was not made: it goes to its dead-letter queue when
// it has one, and is dropped otherwise. origin is the lane of a delivery that
// failed on its transport: when the dead-letter queue's lane is full, origin
// pauses until it has room, up to the deadline, unless that lane is held up
// itself (mayWait). A delivery that failed at a publisher's deadline has no
// origin and does not wait.
func (ls *lanes) fail(j *job, origin *lane, reason string) {
	if j.dlq == nil || j.dead {
		ls.drop(j, reason)

		return
	}

	dead := &job{call: *j.dlq, dead: true, handoff: origin == nil}

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
	admitted := i < l.admitted

	if i == 0 {
		l.jobs[0] = nil
		l.jobs = l.jobs[1:]
	} else {
		l.jobs = slices.Delete(l.jobs, i, i+1)
	}

	j.lane = nil

	if admitted {
		l.admitted--
	}

	if j.waiting {
		j.waiting = false

		if j.admit != nil {
			close(j.admit)
		}

		ls.release(j)
	}

	for l.admitted < ls.depth && l.admitted < len(l.jobs) {
		next := l.jobs[l.admitted]
		l.admitted++
		ls.admitJob(next)
	}

	ls.settle()
}

// settle counts one delivery out of the lanes. The last one out signals idle
// and lets go of the hub. A delivery that fails is counted once more until
// its dead-letter delivery is in a lane, so Close, AwaitDeliveries and the
// hub's drain never see the lanes empty between the two.
func (ls *lanes) settle() {
	ls.queued--
	if ls.queued == 0 {
		close(ls.idle)
		ls.idle = nil
		ls.unhold()
		ls.unhold = nil
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
		for l.admitted < len(l.jobs) {
			j := l.jobs[l.admitted]
			l.admitted++
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

// Close stops delivery at once, without waiting: the shutdown's drain of the
// in-process calls has already made the deliveries it had time for, and
// stopped or cut the client, so nothing sent now could reach a queue. From
// here every delivery is refused. Each delivery still in a lane is accounted
// for once, counted in Dropped and logged: a head the client dropped without
// an answer, or never got, is cut here. Close logs SNS's own count once, the
// deliveries dropped since the process started: the in-process hub's count
// of calls cut is a different number, since a lane has one call in the client
// however many deliveries wait behind it.
func (p *Provider) Close(context.Context) {
	ls := &p.lanes

	ls.mu.Lock()
	ls.closed = true
	before := ls.dropped.Load()
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
	}

	// Cutting a held lane's dead-letter delivery lets go of a lane already
	// passed over above, so every lane is forgotten here.
	ls.byKey = nil
	ls.parked = nil
	ls.inflight = 0
	ls.mu.Unlock()

	ls.log.flush()

	if dropped := ls.dropped.Load(); dropped > 0 {
		slog.Warn("SNS deliveries dropped", "count", dropped, "cut_at_close", dropped-before)
	}
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
