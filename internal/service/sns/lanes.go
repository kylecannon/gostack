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

// logInterval is how often the failed deliveries of one destination are
// logged, as one line with their count.
const logInterval = 5 * time.Second

// The reasons a delivery is dropped, as they are logged.
const (
	reasonClosing    = "SNS is shutting down"
	reasonCut        = "the in-process client stopped before the delivery was made"
	reasonDLQRefused = "the dead-letter queue did not take the message"
)

// lanes deliver SNS's messages, one lane per destination: the deliveries to
// one queue, or one endpoint, wait in its lane and are sent one at a time, in
// the order they were queued, so a destination's deliveries keep the order of
// their publishes, and a slow destination holds up no other.
//
// A lane holds any number of deliveries, as LocalStack's executor queue does:
// a publish adds its deliveries and never waits, and none is refused for
// lack of room, so memory grows while a destination is slow.
//
// A lane has no goroutine of its own. Its head is with the transport, an SQS
// SendMessage queued on the in-process client, and the transport's answer
// sends the next; an empty lane is gone. A parked lane has work and no call in
// the client, so the lanes hold the hub's pending count while any delivery is
// in them (unhold), which is what a shutdown's drain of the client waits for.
//
// Every delivery that enters a lane leaves it once, under mu: sent, failed
// (to its dead-letter queue or dropped), or cut. queued counts the ones in a
// lane, and dropped the ones lost.
type lanes struct {
	client *inproc.Client

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
	// in the client: one waiting in its lane, in a parked lane, or added and
	// not yet started.
	unhold func()
	// closed is set when Close begins.
	closed bool

	dropped atomic.Int64
	log     dropLog
}

// lane is the deliveries to one destination, oldest first.
type lane struct {
	key  string
	jobs []*job
	// sending is set while jobs[0] is with the transport.
	sending bool
	parked  bool
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
}

// add puts j at the tail of its destination's lane, and reports whether it is
// there. Only Close refuses a delivery: one added after it has begun is
// dropped, counted and logged.
func (ls *lanes) add(j *job) bool {
	if ls.closed {
		ls.drop(j, reasonClosing)

		return false
	}

	l := ls.byKey[j.call.Lane]
	if l == nil {
		l = &lane{key: j.call.Lane}
		l.done = func(resp inproc.Response, err error) { ls.answered(l, resp, err) }

		if ls.byKey == nil {
			ls.byKey = make(map[string]*lane)
		}

		ls.byKey[l.key] = l
	}

	j.lane = l
	l.jobs = append(l.jobs, j)

	if ls.queued == 0 {
		ls.idle = make(chan struct{})
		ls.unhold = ls.client.Hold()
	}

	ls.queued++

	return true
}

// dispatch starts the lanes of a publish's deliveries, with no topic or FIFO
// lock held, and returns: it never waits for a delivery. The order was fixed
// when the deliveries were added.
func (ls *lanes) dispatch(jobs []*job) {
	if len(jobs) == 0 {
		return
	}

	ls.mu.Lock()
	defer ls.mu.Unlock()

	for _, j := range jobs {
		if j.lane != nil {
			ls.start(j.lane)
		}
	}
}

// start hands l's head to the transport when the lane is not sending or
// parked already. When the client refuses the head with a call of SNS's still
// in it, its queue is full, and the lane is parked until an answer frees room;
// with none, the client was stopped, and every delivery in the lane is cut.
func (ls *lanes) start(l *lane) {
	if ls.closed || l.sending || l.parked || len(l.jobs) == 0 {
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
		ls.drop(ls.removeHead(l), reasonCut)
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
// sends the next. The lane goes on, and the head is counted out, even when
// failing the head panics: the panic is not recovered here, so the worker's
// recover logs it once, but the lane's other deliveries are not left behind,
// and the hub's drain is not held to its deadline by their hold.
func (ls *lanes) answered(l *lane, resp inproc.Response, err error) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	ls.inflight--
	l.sending = false

	if len(l.jobs) == 0 {
		// Close cut it already.
		return
	}

	ls.queued++ // see settle
	j := ls.removeHead(l)

	defer ls.settle()
	defer ls.proceed(l) // before settle, as it was when it ended answered

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

		ls.fail(j, reason)
	}
}

// proceed is what follows an answer to l's head: l is forgotten when the
// shutdown has begun, and goes on otherwise, after the parked lanes. They go
// first, so a lane with more to send does not take the room its answer freed
// from them again and again; it is parked behind them when none is left.
func (ls *lanes) proceed(l *lane) {
	if ls.closed {
		ls.tidy(l)

		return
	}

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

// fail is a delivery the destination did not take: it goes to the tail of
// its dead-letter queue's lane when it has one, and is dropped otherwise. A
// dead-letter delivery that fails is dropped, so the chain of deliveries one
// publish makes is finite.
func (ls *lanes) fail(j *job, reason string) {
	if j.dlq == nil || j.dead {
		ls.drop(j, reason)

		return
	}

	dead := &job{call: *j.dlq, dead: true}
	if !ls.add(dead) {
		return
	}

	ls.log.deadLettered(j.call.Lane, reason)
	ls.start(dead.lane)
}

// removeHead takes l's head out of the lane, which it leaves for good, and
// counts it out of queued.
func (ls *lanes) removeHead(l *lane) *job {
	j := l.jobs[0]
	l.jobs[0] = nil
	l.jobs = l.jobs[1:]
	j.lane = nil

	ls.settle()

	return j
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
	if len(l.jobs) == 0 && !l.sending && !l.parked && ls.byKey[l.key] == l {
		delete(ls.byKey, l.key)
	}
}

// drop counts and logs a delivery that is lost.
func (ls *lanes) drop(j *job, reason string) {
	ls.dropped.Add(1)
	ls.log.dropped(j.call.Lane, reason)
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
			ls.drop(ls.removeHead(l), reasonCut)
		}
	}

	ls.byKey = nil
	ls.parked = nil
	ls.inflight = 0
	ls.mu.Unlock()

	ls.log.flush()

	if dropped := ls.dropped.Load(); dropped > 0 {
		slog.Warn("SNS deliveries dropped", "count", dropped, "cut_at_close", dropped-before)
	}
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
