package protocol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
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
	// the bodies being buffered at once may hold in all. Four bodies at
	// DefaultMaxBody, each decoding into at most DecodeBudget of itself, hold
	// about 4 x (16 + 48) MiB, which a 512 MiB container survives with the
	// runtime and the services' state beside it.
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

// SetBodyWait sets how long a body waits for room under the bound. A test that
// changes it puts the old value back.
func SetBodyWait(wait time.Duration) {
	bodyWait.Store(int64(wait))
}

// bodyPool is a weighted semaphore of bytes. Waiters are served in order, a
// body that already holds some of the pool ahead of one that holds none, so
// that bodies part way through their read finish and give their bytes back
// instead of waiting on bodies that have not started.
type bodyPool struct {
	mu       sync.Mutex
	capacity int64
	used     int64
	growing  []*bodyWaiter
	starting []*bodyWaiter
}

type bodyWaiter struct {
	n     int64
	ready chan struct{}
}

// acquire takes n bytes of the pool, or waits for them in queue, which is
// &p.growing or &p.starting.
func (p *bodyPool) acquire(ctx context.Context, n int64, queue *[]*bodyWaiter) error {
	p.mu.Lock()

	if len(p.growing) == 0 && len(*queue) == 0 && p.used+n <= p.capacity {
		p.used += n
		p.mu.Unlock()

		return nil
	}

	w := &bodyWaiter{n: n, ready: make(chan struct{})}
	*queue = append(*queue, w)
	p.mu.Unlock()

	timer := time.NewTimer(time.Duration(bodyWait.Load()))
	defer timer.Stop()

	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
	case <-timer.C:
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	select {
	case <-w.ready: // granted as it gave up: give it back
		p.used -= n
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

// grant lets waiters in, in order, while the one at the head fits.
func (p *bodyPool) grant() {
	for _, queue := range []*[]*bodyWaiter{&p.growing, &p.starting} {
		for len(*queue) > 0 {
			w := (*queue)[0]
			if p.used+w.n > p.capacity {
				return
			}

			p.used += w.n
			close(w.ready)
			(*queue)[0] = nil
			*queue = (*queue)[1:]
		}
	}
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
	length int64
	read   int64
	held   int64
	armed  bool
	done   bool
	err    error

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

// Read reads the body and, once the meter is armed, charges it past FreeBody:
// a body that states its length is charged all of it at once, the first time
// it reads past FreeBody, and one that states none in steps that double. When
// the bound leaves no room before the wait ends, it gives back what the body
// held and fails with ErrBusy.
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
			// the connection is closed after the answer.
			m.timed = false
			m.Release()
			m.err = fmt.Errorf("%w: %w", ErrBodyIdle, err)

			return 0, m.err
		case errors.Is(err, io.EOF):
			m.clearDeadline()
		default:
			// A body still arriving keeps its deadline for the next read.
		}
	}

	if !m.armed || m.done || n == 0 {
		return n, err
	}

	m.read += int64(n)
	if m.read <= FreeBody || m.read <= m.held || m.held >= m.most {
		return n, err
	}

	const growth = 2

	queue := &m.pool.starting
	if m.held > 0 {
		queue = &m.pool.growing
	}

	want := min(max(growth*m.held, m.read, growth*FreeBody), m.most)
	if m.held == 0 && m.length > 0 {
		// All or nothing: a body that holds part of the bound never waits
		// for the rest while others hold the remainder.
		want = min(max(m.length, m.read), m.most)
	}

	busy := m.pool.acquire(m.ctx, want-m.held, queue)
	if busy != nil {
		m.Release()
		m.err = busy

		return 0, busy
	}

	m.held = want

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
