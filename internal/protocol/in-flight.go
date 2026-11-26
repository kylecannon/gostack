package protocol

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kylecannon/gostack/internal/awserr"
)

const (
	// FreeBody is how many bytes of a request body are read before the body is
	// charged against the in-flight bound (SetInFlight). A request that states a
	// length up to FreeBody is never metered at all: the gateway leaves its body
	// as it is, so it takes no lock and no atomic.
	FreeBody = 64 << 10

	// DefaultInFlight is GOSTACK_MAX_IN_FLIGHT_BODIES when it is not set: what
	// the bodies being buffered at once and their decoding may hold in all:
	// four form bodies at DefaultMaxBody, or one json or xml body at it with
	// the 32 MiB of decoding it is admitted with. One request's decoding is
	// charged 48 MiB at most, so what a 512 MiB container holds for requests
	// stays far below it, beside the runtime and the services' state.
	DefaultInFlight int64 = 64 << 20

	// DefaultBodyWait is how long a body waits for room under the bound before
	// its request is answered with the protocol's throttling error.
	DefaultBodyWait = 5 * time.Second

	// DefaultBodyIdle is GOSTACK_REQUEST_BODY_IDLE_TIMEOUT when it is not set:
	// how long a metered body may send nothing before its request is answered
	// and what it holds is given back. A client that sends at all sends a 16 MiB
	// body's next packet well within it: 20 s is room for a stalled mobile link
	// or a paused debugger, and still frees the pool from a client that stopped.
	DefaultBodyIdle = 20 * time.Second
)

// ErrBodyIdle is what reading a metered body fails with when its client sent
// nothing for the idle timeout (SetBodyIdle). S3 answers it with RequestTimeout;
// the json and query services answer it as any body they cannot read.
var ErrBodyIdle = errors.New("protocol: the request body sent nothing for the idle timeout")

// RequestTimeout is S3's answer to a body that stopped arriving, with AWS's
// code and message.
func RequestTimeout() *awserr.Error {
	return &awserr.Error{
		Code: "RequestTimeout",
		Message: "Your socket connection to the server was not read from or written to within " +
			"the timeout period. Idle connections will be closed.",
		StatusCode: http.StatusBadRequest, Fault: awserr.FaultSender,
	}
}

// ErrBusy is what reading a metered body fails with when the bodies in flight
// leave it no room before its wait ends, or its request is cancelled while it
// waits. A service answers it with its protocol's throttling error, which SDKs
// retry.
var ErrBusy = errors.New("protocol: the request bodies in flight are at their bound")

// ErrBodyLong is what reading a metered body fails with once it states or has
// read more than its service takes (BoundBody). The read past FreeBody that
// finds it fails, holding nothing, so a body that states a longer length is
// refused before it is admitted, and one of unknown length holds no more than
// the bound. The service answers it as a request too long to be valid.
var ErrBodyLong = errors.New("protocol: the request body is longer than its service takes")

// BoundBody makes the metered body of the request whose context is ctx fail
// with ErrBodyLong past limit bytes, for a service none of whose valid requests
// is longer: the gateway calls it once it knows the service, before the body
// is read. A body charged already, one that a form read to find its service,
// keeps what it holds and is not bounded: its service judges it once read.
func BoundBody(ctx context.Context, limit int64) {
	c := ChargeOf(ctx)
	if c == nil || c.meter == nil || c.meter.read > FreeBody || limit <= 0 {
		return
	}

	c.meter.limit = limit
	c.meter.most = min(c.meter.most, limit)
}

// The answers to a request whose body failed with ErrBusy, one per protocol.
// Each code is one the AWS SDKs retry as throttling (aws-sdk-go-v2's
// retry.DefaultThrottleErrorCodes, botocore's _retry.json "throttling"
// definitions): Throttling for query (STS's own), ThrottlingException for json,
// and S3's SlowDown with its 503.

// ThrottledJSON is the answer in the json protocols.
func ThrottledJSON() *awserr.Error {
	return &awserr.Error{
		Code: "ThrottlingException", Message: "Rate exceeded",
		StatusCode: http.StatusBadRequest, Fault: awserr.FaultSender,
	}
}

// ThrottledQuery is the answer in the query protocol.
func ThrottledQuery() *awserr.Error {
	return &awserr.Error{
		Code: "Throttling", Message: "Rate exceeded",
		StatusCode: http.StatusBadRequest, Fault: awserr.FaultSender,
	}
}

// SlowDown is S3's answer.
func SlowDown() *awserr.Error {
	return &awserr.Error{
		Code: "SlowDown", Message: "Please reduce your request rate.",
		StatusCode: http.StatusServiceUnavailable, Fault: awserr.FaultReceiver,
	}
}

var (
	inFlight atomic.Pointer[bodyPool]
	bodyWait = newDuration(DefaultBodyWait)
	bodyIdle = newDuration(DefaultBodyIdle)
)

func newDuration(d time.Duration) *atomic.Int64 {
	v := new(atomic.Int64)
	v.Store(int64(d))

	return v
}

// SetBodyIdle sets how long a metered body may send nothing before its read
// fails with ErrBodyIdle. Zero or less means no limit. Start-up calls it once;
// a test that changes it puts the old value back.
func SetBodyIdle(idle time.Duration) {
	bodyIdle.Store(int64(max(idle, 0)))
}

// SetInFlight sets the in-flight bound: the bytes that the request bodies being
// buffered at once may hold. Zero or less means no bound. A bound below twice
// one byte more than MaxBody is raised to it, so that every body MaxBody admits
// fits with at least as much room again for what it decodes into: decoding is
// charged up to the bound less one body (DecodeHold), never zero. Start-up
// calls it once, after SetMaxBody and before the gateway serves; a test that
// changes it puts the old value back.
func SetInFlight(bound int64) {
	if bound <= 0 {
		inFlight.Store(nil)

		return
	}

	if limit := MaxBody(); limit > 0 {
		bound = max(bound, 2*(limit+1)) //nolint:mnd // a body and as much again for its decoding
	}

	inFlight.Store(&bodyPool{capacity: bound})
}

// InFlight returns the in-flight bound and what is charged against it now. Both
// are zero when there is no bound.
func InFlight() (bound, held int64) {
	pool := inFlight.Load()
	if pool == nil {
		return 0, 0
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()

	return pool.capacity, pool.used
}

// BodiesWait reports whether a request body, or a decoding, would still wait
// for room under the in-flight bound were own bytes of what the pool holds
// given back: whether the waiters, in the order they are let in, need more
// than the room that leaves. A caller that holds own bytes and is about to
// give them back asks it, so that a body waiting only on those bytes does not
// count. It is false when there is no bound.
func BodiesWait(own int64) bool {
	pool := inFlight.Load()
	if pool == nil {
		return false
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()

	room := pool.capacity - (pool.used - own)

	for i, queue := range [][]*bodyWaiter{pool.steps, pool.growing, pool.starting} {
		for _, w := range queue {
			// A step that waits for a safe order waits whatever the caller
			// gives back, fits or not, and holds up no one (grant).
			if pool.blocked(w) {
				if i == 0 {
					continue
				}

				return true
			}

			if room < w.n {
				return true
			}

			room -= w.n
		}
	}

	return false
}

// SetBodyWait sets how long a body waits for room under the bound. A test that
// changes it puts the old value back.
func SetBodyWait(wait time.Duration) {
	bodyWait.Store(int64(wait))
}

// bodyPool is a weighted semaphore of bytes. Waiters are served in order, a
// request that already holds some of the pool ahead of one that holds none, so
// that requests part way through finish and give their bytes back instead of
// waiting on bodies that have not started: the steps of bodies of unknown
// length still being read (steps), oldest body first, then the decodings that
// need more (growing), then bodies not yet admitted (starting), each in the
// order they came.
//
// A body that states its length is charged in one step (admit), and a decoding
// that needs more gives back the decode room it holds and waits for all it
// needs (regrow). Its request's body bytes stay held while it waits, and
// p.growing is served strictly in order: a decoding at its head that does not
// fit beside the bodies of the decodings behind it waits, and the queues behind
// it with it, until one of those requests ends, at the latest at its own wait.
//
// A body of unknown length holds only what it has read, and grows in steps
// (BodyMeter.grow) while it holds the steps before; each step is granted only
// when, after it, every such body could still reach its claim, the most it may
// hold, in some order (safe), counting what every other request holds as given
// back once it finishes. So some such body can always finish, and a step that
// waits for one to finish lets the steps of the others pass it (grant). A
// body's first step waits in p.starting, and one that waits for a safe order
// holds up the bodies queued behind it there.
type bodyPool struct {
	mu       sync.Mutex
	capacity int64
	used     int64
	steps    []*bodyWaiter
	growing  []*bodyWaiter
	starting []*bodyWaiter
	// growers are the bodies of unknown length being read that hold some of
	// the pool, in the order they were admitted; seq numbers them.
	growers []*grower
	seq     uint64
}

// grower is the pool's record of a body of unknown length being read: what it
// holds of the pool, its body's step and the decode room planned for it, and
// its claim, the most it may hold. It is changed only under the pool's lock.
type grower struct {
	held   int64
	claim  int64
	seq    uint64
	joined bool
}

type bodyWaiter struct {
	n     int64
	g     *grower // the body of unknown length n is a step of, or nil
	ready chan struct{}
	// bare marks a waiter whose request holds nothing of the pool: a body
	// queued for admission, or a decoding whose request holds no body bytes
	// and no decode room. take gives it the shutdown stop (StopQueued), which
	// then ends its wait.
	bare bool
	stop <-chan struct{}
}

// acquire takes n bytes of the pool, or waits for them in queue, which is
// &p.growing or &p.starting.
func (p *bodyPool) acquire(ctx context.Context, n int64, queue *[]*bodyWaiter) error {
	return p.take(ctx, &bodyWaiter{n: n, ready: make(chan struct{})}, queue)
}

// step takes n more bytes for the body of unknown length g, in p.steps once g
// holds some of the pool and in p.starting until then, as bodyPool describes.
// bare reports that the request holds nothing of the pool but what g holds.
func (p *bodyPool) step(ctx context.Context, g *grower, n int64, bare bool) error {
	w := &bodyWaiter{n: n, g: g, ready: make(chan struct{})}

	p.mu.Lock()
	joined := g.joined
	p.mu.Unlock()

	if joined {
		return p.take(ctx, w, &p.steps)
	}

	w.bare = bare

	return p.take(ctx, w, &p.starting)
}

// take queues w in queue, lets in what may come in, and waits for w unless it
// came in. A step joins p.steps ahead of the steps of younger bodies.
func (p *bodyPool) take(ctx context.Context, w *bodyWaiter, queue *[]*bodyWaiter) error {
	w.watchStop()

	p.mu.Lock()

	if queue == &p.steps {
		i := 0
		for i < len(p.steps) && p.steps[i].g.seq < w.g.seq {
			i++
		}

		p.steps = slices.Insert(p.steps, i, w)
	} else {
		*queue = append(*queue, w)
	}

	p.grant()
	p.mu.Unlock()

	select {
	case <-w.ready:
		return nil
	default:
	}

	return p.wait(ctx, w, queue)
}

// safe reports whether, were g to hold n bytes more, every body of unknown
// length could still reach its claim in some order (the banker's check): what
// the pool has left once every other request gives back what it holds covers
// the need of one of them, its bytes, given back, cover the next one's with
// it, and so on. With one resource, taking them in order of their need is such
// an order whenever any is.
func (p *bodyPool) safe(g *grower, n int64) bool {
	type claim struct{ held, need int64 }

	claims := make([]claim, 0, len(p.growers)+1)
	free := p.capacity - n

	for _, o := range p.growers {
		held := o.held
		if o == g {
			held += n
		}

		free -= o.held
		claims = append(claims, claim{held: held, need: o.claim - held})
	}

	if !g.joined {
		claims = append(claims, claim{held: n, need: g.claim - n})
	}

	slices.SortFunc(claims, func(a, b claim) int { return cmp.Compare(a.need, b.need) })

	for _, c := range claims {
		if c.need > free {
			return false
		}

		free += c.held
	}

	return true
}

// blocked reports whether w is a step of a body of unknown length that would
// leave the pool unsafe (safe).
func (p *bodyPool) blocked(w *bodyWaiter) bool {
	return w.g != nil && !p.safe(w.g, w.n)
}

// finish takes g out of the bodies of unknown length being read, at its end
// or when it is released, and gives back n bytes: what it holds from then on
// is only given back.
func (p *bodyPool) finish(g *grower, n int64) {
	p.mu.Lock()

	if i := slices.Index(p.growers, g); i >= 0 {
		p.growers = slices.Delete(p.growers, i, i+1)
	}

	g.joined = false
	p.used -= n
	p.grant()
	p.mu.Unlock()
}

// regrow makes what a holder of held bytes holds want bytes, want more than
// held: at once when no one waits ahead of it and the room is there, and
// otherwise it gives held back and waits in p.growing for all of want, in the
// order the growing waiters came. The give-back and the joining happen under
// one lock, so a body that arrives meanwhile cannot take the bytes given back.
// bare reports that the request holds nothing else of the pool. On ErrBusy the
// holder holds nothing.
func (p *bodyPool) regrow(ctx context.Context, held, want int64, bare bool) error {
	p.mu.Lock()

	if len(p.steps) == 0 && len(p.growing) == 0 && p.used+want-held <= p.capacity {
		p.used += want - held
		p.mu.Unlock()

		return nil
	}

	p.used -= held
	w := &bodyWaiter{n: want, ready: make(chan struct{}), bare: bare}
	w.watchStop()
	p.growing = append(p.growing, w)
	p.grant()
	p.mu.Unlock()

	return p.wait(ctx, w, &p.growing)
}

// queuedStop is the channel StopQueued set, or nil.
var queuedStop atomic.Pointer[<-chan struct{}]

// watchStop makes a bare waiter's wait end when the shutdown stop closes
// (StopQueued).
func (w *bodyWaiter) watchStop() {
	if !w.bare {
		return
	}

	if stop := queuedStop.Load(); stop != nil {
		w.stop = *stop
	}
}

// StopQueued sets the channel whose close ends the waits of the requests that
// hold nothing of the in-flight bound, the bodies queued for admission and
// the decodings of requests that hold neither body bytes nor decode room:
// each fails with ErrBusy. A request that holds some of the bound keeps
// waiting for its next step until its own context or the body wait ends it.
// The gateway hands over the channel it closes as it begins to shut down, so a
// body it queued is answered with its service's throttle within the drain, not
// cut when the drain ends. It holds for whatever bound SetInFlight sets, before
// or after; a nil channel stops nothing.
func StopQueued(stop <-chan struct{}) {
	queuedStop.Store(&stop)
}

// wait waits for w, in queue, to be granted, until ctx ends, the body wait
// (SetBodyWait) passes or, for a request that holds nothing (bodyWaiter.bare),
// the shutdown stop (StopQueued) closes; then it takes w out of queue, or gives
// back what it was granted as it gave up, and fails with ErrBusy.
func (p *bodyPool) wait(ctx context.Context, w *bodyWaiter, queue *[]*bodyWaiter) error {
	n := w.n

	timer := time.NewTimer(time.Duration(bodyWait.Load()))
	defer timer.Stop()

	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
	case <-timer.C:
	case <-w.stop:
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	select {
	case <-w.ready: // granted as it gave up: give it back
		p.used -= n
		if w.g != nil {
			w.g.held -= n
		}
	default:
		*queue = removeWaiter(*queue, w)
	}

	p.grant()

	return ErrBusy
}

func (p *bodyPool) release(n int64) {
	p.mu.Lock()
	p.used -= n
	p.grant()
	p.mu.Unlock()
}

// grant lets waiters in, in order, while the one at the head fits and, for a
// step of a body of unknown length, leaves the pool safe. A body's first step
// makes it the youngest of the bodies of unknown length being read.
//
// The steps are scanned oldest body first. A step that is not safe waits for
// another such body to read on or end, not for room, so it is passed over
// whether or not it fits: a younger body's step that is safe and fits is let
// in ahead of it, and the scan starts again from the oldest, since that may
// have made it safe (nextStep). A safe step that does not fit stops the scan
// and everything behind it, so nothing younger takes the room it waits for.
func (p *bodyPool) grant() {
	for {
		i, short := p.nextStep()
		if short {
			return
		}

		if i < 0 {
			break
		}

		p.let(&p.steps, i)
	}

	for _, queue := range []*[]*bodyWaiter{&p.growing, &p.starting} {
		for len(*queue) > 0 {
			w := (*queue)[0]
			if p.blocked(w) || p.used+w.n > p.capacity {
				return
			}

			p.let(queue, 0)
		}
	}
}

// nextStep returns where in p.steps the oldest step that leaves the pool safe
// and fits is, or -1 when there is none; short reports that a safe step older
// than any such one does not fit. Safety is tested first: a step that is not
// safe waits for another body to read on or end whether or not it fits, and
// room given to it would not let it in.
func (p *bodyPool) nextStep() (i int, short bool) {
	for i, w := range p.steps {
		if p.blocked(w) {
			continue
		}

		if p.used+w.n > p.capacity {
			return -1, true
		}

		return i, false
	}

	return -1, false
}

// let grants the waiter at i in queue and takes it out of queue.
func (p *bodyPool) let(queue *[]*bodyWaiter, i int) {
	w := (*queue)[i]

	p.used += w.n
	if g := w.g; g != nil {
		if !g.joined {
			p.seq++
			g.seq, g.joined = p.seq, true
			p.growers = append(p.growers, g)
		}

		g.held += w.n
	}

	close(w.ready)
	*queue = slices.Delete(*queue, i, i+1)
}

func removeWaiter(queue []*bodyWaiter, w *bodyWaiter) []*bodyWaiter {
	for i := range queue {
		if queue[i] == w {
			return append(queue[:i], queue[i+1:]...)
		}
	}

	return queue
}

// BodyMeter charges a request body against the in-flight bound as it is read,
// once something that buffers the body (ReadRequest, rest.CapBody) arms it
// through the request's context (ArmBody), however the body has been wrapped
// since. Reading a body that is streamed (an S3 object) leaves it unarmed, so
// it is never charged. It charges the length the body states, or what has been
// read in steps that double when it states none, up to its share of the bound
// (bodyShare), and holds it until Release. A BodyMeter is read and released on
// the request's own goroutine.
type BodyMeter struct {
	body   io.ReadCloser
	ctx    context.Context //nolint:containedctx // the request's, for the wait while it is read
	pool   *bodyPool
	most   int64
	limit  int64 // the most its service takes (BoundBody), zero for none
	length int64
	read   int64
	held   int64
	armed  bool
	done   bool
	err    error

	// plan is what decoding holds for each byte of the body (TextDecode,
	// FormDecode), set when it is armed; decode is the request's decode hold
	// (Charge.Meter), which the room planned is charged to.
	plan   int64
	decode *DecodeHold
	// grower is a body of unknown length's record in the pool while it is
	// read (grow); prepaid is the decode room its steps took for its decoding,
	// which trim cuts to what it read, and rooms whether its steps take any.
	grower  *grower
	prepaid int64
	rooms   bool

	// deadline sets the connection's read deadline (IdleBy); timed is whether
	// one is set now.
	deadline func(time.Time) error
	timed    bool
}

// NewBodyMeter meters body, which states length bytes (-1 when it states none),
// against the in-flight bound for a request whose context is ctx, or returns
// nil when there is no bound. Charge.Meter puts it on the request's Charge, for
// ArmBody and Release. The gateway calls it for a request from a listener whose
// body is longer than FreeBody or of unknown length.
func NewBodyMeter(ctx context.Context, body io.ReadCloser, length int64) *BodyMeter {
	pool := inFlight.Load()
	if pool == nil {
		return nil
	}

	return &BodyMeter{body: body, ctx: ctx, pool: pool, most: pool.bodyShare(), length: length}
}

// bodyShare is the most one body is charged: one byte more than MaxBody, or
// half the bound when there is no MaxBody. SetInFlight keeps it to half the
// bound at most, so what one request decodes always has room beside its body.
func (p *bodyPool) bodyShare() int64 {
	if limit := MaxBody(); limit > 0 {
		return min(limit+1, p.capacity)
	}

	return p.capacity / 2 //nolint:mnd // half for the body, half for what it decodes into
}

// Read reads the body and, once the meter is armed, charges it past FreeBody.
// A body that states its length is charged all of it at once, the first time
// it reads past FreeBody, together with the room its decoding is planned to
// hold (admit). One that states none is charged in steps that double, each
// with the room planned for decoding it (grow), and at its end keeps only what
// it read and the room planned for that (trim). When the bound leaves no room
// before the wait ends, it holds nothing and fails with ErrBusy.
func (m *BodyMeter) Read(p []byte) (int, error) {
	if m.err != nil {
		return 0, m.err
	}

	if m.armed && !m.done {
		m.idle()
	}

	n, err := m.body.Read(p)
	if m.timed {
		switch {
		case errors.Is(err, os.ErrDeadlineExceeded):
			// The deadline stays in the past. net/http reads what is left of
			// a short body before it writes the answer; with the deadline
			// cleared that read would wait for the client, and the answer
			// would never be sent. With it past, the read fails at once and
			// the connection is closed after the answer. The body will not
			// be decoded, so the room planned for that is given back too.
			m.timed = false
			m.Release()
			m.decode.Release()
			m.err = fmt.Errorf("%w: %w", ErrBodyIdle, err)

			return 0, m.err
		case errors.Is(err, io.EOF):
			m.clearDeadline()
		default:
			// A body still arriving keeps its deadline for the next read.
		}
	}

	if !m.armed || m.done {
		return n, err
	}

	if n > 0 {
		busy := m.charge(int64(n))
		if busy != nil {
			return 0, busy
		}
	}

	if errors.Is(err, io.EOF) && m.grower != nil {
		m.trim()
	}

	return n, err
}

// Close closes the body.
func (m *BodyMeter) Close() error {
	return m.body.Close()
}

// Release gives back what the body holds and stops charging it. The gateway
// calls it when the handler returns; a handler that holds its request open
// after it has decoded the body (an SQS long poll) calls ReleaseBody first.
func (m *BodyMeter) Release() {
	if m == nil || m.done {
		return
	}

	m.done = true
	m.clearDeadline()

	if m.grower != nil {
		m.pool.finish(m.grower, m.held)
		m.grower, m.held = nil, 0

		return
	}

	if m.held > 0 {
		m.pool.release(m.held)
		m.held = 0
	}
}

// IdleBy makes the meter bound how long its client may send nothing, once it
// is armed: before each read it sets the connection's read deadline through set
// (http.ResponseController.SetReadDeadline) to the idle timeout ahead
// (SetBodyIdle), and it clears it at the end of the body and at Release, so
// nothing after the body, an SQS long poll or the connection's next request,
// meets it. An S3 object stream is never armed, so never bounded here.
func (m *BodyMeter) IdleBy(set func(time.Time) error) {
	m.deadline = set
}

// trim makes a body of unknown length, at its end, hold what it read, and the
// room planned for its decoding what a body that stated that length is
// admitted with, unless decoding has charged more of it already or a Cover has
// returned it as covered (DecodeHold.spent), and takes it out of the bodies
// being read (bodyPool.finish).
func (m *BodyMeter) trim() {
	release := max(m.held-m.read, 0)
	m.held -= release

	if m.prepaid > 0 && m.decode.held == m.prepaid && !m.decode.spent {
		room := min(decodeRoom(m.pool, m.plan*m.read+FreeBody), m.prepaid)
		release += m.prepaid - room
		m.decode.held, m.prepaid = room, room
	}

	m.pool.finish(m.grower, release)
	m.grower = nil
}

// idle sets the read deadline for the next read, when IdleBy gave a way to.
func (m *BodyMeter) idle() {
	idle := time.Duration(bodyIdle.Load())
	if m.deadline == nil || idle <= 0 {
		return
	}

	// A writer that cannot set deadlines (http.ErrNotSupported) leaves the
	// body unbounded in time, as it was before.
	if m.deadline(time.Now().Add(idle)) == nil {
		m.timed = true
	}
}

// clearDeadline clears the read deadline idle set.
func (m *BodyMeter) clearDeadline() {
	if m.timed {
		m.timed = false
		_ = m.deadline(time.Time{})
	}
}

// charge charges n more bytes read, as Read describes.
func (m *BodyMeter) charge(n int64) error {
	m.read += n
	if m.read <= FreeBody {
		return nil
	}

	if m.limit > 0 && max(m.read, m.length) > m.limit {
		m.Release()
		m.decode.Release()
		m.err = ErrBodyLong

		return m.err
	}

	if m.read <= m.held || m.held >= m.most {
		return nil
	}

	if m.length < 0 {
		return m.grow()
	}

	return m.admit()
}

// admit charges a body that states its length all of it, and the room its
// decoding is planned to hold (DecodeHold.planned), in one step, in the order
// bodies arrive: a body that holds part of the bound never waits for more while
// others hold the rest, whether for its own bytes or for decoding them. A body
// over MaxBody is refused once read, so nothing is planned for its decoding.
func (m *BodyMeter) admit() error {
	want := min(max(m.length, m.read), m.most)

	var room int64
	if m.decode != nil && m.length < m.most {
		room = m.decode.planned(m.pool, m.plan*m.length+FreeBody)
	}

	w := &bodyWaiter{n: want + room, ready: make(chan struct{}), bare: m.decode.Held() == 0}

	busy := m.pool.take(m.ctx, w, &m.pool.starting)
	if busy != nil {
		m.Release()
		m.err = busy

		return busy
	}

	m.held = want
	m.decode.prepaid(m.pool, room)

	return nil
}

// grow charges a body that states no length its next step: twice what it holds
// or what it has read, up to its share of the bound (bodyShare), with the room
// planned for decoding that much (decodeRoom), which the request's decode hold
// keeps. Its claim, the most it may hold, is its share and the room planned
// for that, so a body that reaches its end never waits for room to decode
// while it holds its bytes. The pool grants the step once it leaves every body
// of unknown length a safe way to its claim (bodyPool.safe). When the step
// does not come before the wait ends, the body gives back all it holds, its
// decode room too, and fails with ErrBusy.
func (m *BodyMeter) grow() error {
	const growth = 2

	if m.grower == nil {
		var room int64
		if m.decode != nil {
			room = m.decode.planned(m.pool, m.plan*m.most+FreeBody)
		}

		m.grower, m.rooms = &grower{claim: m.most + room}, room > 0
	}

	body := min(max(growth*m.held, m.read, growth*FreeBody), m.most)

	var room int64
	if m.rooms {
		room = decodeRoom(m.pool, m.plan*body+FreeBody)
	}

	// The step takes only the room past what the decode hold holds now: a
	// decoding that charges as the body is read (restxml) may have covered
	// more between two steps (Cover), and those bytes stay the hold's.
	hold := m.decode.Held()
	more := max(room-hold, 0)

	busy := m.pool.step(m.ctx, m.grower, body-m.held+more, hold == 0)
	if busy != nil {
		m.Release()
		m.decode.Release()
		m.err = busy

		return busy
	}

	m.held = body
	if more > 0 {
		m.decode.prepaid(m.pool, room)
	}

	// What the steps took is still all the hold holds only while Cover has
	// not changed it; once it has, decoding owns the hold, and trim leaves it.
	if hold == m.prepaid {
		m.prepaid = m.decode.Held()
	} else {
		m.prepaid = 0
	}

	return nil
}
