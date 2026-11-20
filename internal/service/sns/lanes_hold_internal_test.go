package sns

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/inproc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// laneCall is a SendMessage in lane with body.
func laneCall(lane, body string) inproc.Call {
	return inproc.Call{Service: sqsService, Target: sqsSendMessage, Lane: lane, Body: []byte(body)}
}

// Test_lanes_drainSeesQueuedDeliveries: a delivery added to a lane and not yet
// handed to the client counts in the hub's pending count, so Hub.Drain waits
// for it instead of returning at once.
func Test_lanes_drainSeesQueuedDeliveries(t *testing.T) {
	p := New(nil)

	var served atomic.Int64

	hub := inproc.NewHub(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	p.Bind(hub)
	t.Cleanup(func() { p.Close(t.Context()) })

	topic, deliveries := laneFixture(20, 2)
	jobs := p.queue(topic, deliveries)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	drained := make(chan struct{})

	go func() {
		defer close(drained)

		hub.Drain(ctx)
	}()

	select {
	case <-drained:
		t.Fatal("Drain returned with 20 deliveries in the lanes and none sent")
	case <-time.After(50 * time.Millisecond):
	}

	p.lanes.dispatch(jobs)
	<-drained

	require.NoError(t, ctx.Err(), "Drain waited for its deadline")
	assert.Equal(t, int64(20), served.Load())
	assert.Zero(t, p.Dropped())
}

// Test_lanes_drainWaitsForHeldLanes: two lanes whose every send fails, each
// the other's dead-letter queue (the review's driver, SQ-304). Hub.Drain
// returns once both lanes are empty, before its deadline, and not while they
// still hold deliveries.
func Test_lanes_drainWaitsForHeldLanes(t *testing.T) {
	p := New(nil)
	p.lanes.depth, p.lanes.wait = 2, 2*time.Second

	hub := inproc.NewHub(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	p.Bind(hub)
	t.Cleanup(func() { p.Close(t.Context()) })

	sub := &Subscription{Endpoint: "e"}
	topic := &Topic{ARN: "t", endpoints: map[string]*Subscription{"e": sub}}

	deliveries := make([]delivery, 0, 12)

	for range 6 {
		for _, pair := range [][2]string{{"a", "b"}, {"b", "a"}} {
			dlq := laneCall(pair[1], "{}")
			deliveries = append(deliveries, delivery{sub: sub, call: laneCall(pair[0], "{}"), dlq: &dlq})
		}
	}

	published := make(chan struct{})

	go func() {
		defer close(published)

		p.lanes.dispatch(p.queue(topic, deliveries))
	}()

	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	hub.Drain(ctx)
	require.NoError(t, ctx.Err(), "Drain waited for its deadline")

	p.lanes.mu.Lock()
	left := p.lanes.queued
	p.lanes.mu.Unlock()

	assert.Zero(t, left, "Drain returned with deliveries still in the lanes")
	<-published
	assert.Equal(t, int64(12), p.Dropped(), "every delivery and its dead letter failed")
}

// Test_lanes_closeForgetsHeldLanes: a lane waits for room in a full
// dead-letter queue's lane, behind a publisher's delivery, when Close cuts
// both. No lane is left behind, whichever lane Close passes first.
func Test_lanes_closeForgetsHeldLanes(t *testing.T) {
	for iter := range 20 {
		p := New(nil)
		p.lanes.depth, p.lanes.wait = 2, time.Hour

		p.Bind(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if bytes.Equal(body, []byte("fail")) {
				w.WriteHeader(http.StatusInternalServerError)

				return
			}

			<-r.Context().Done()
			w.WriteHeader(http.StatusServiceUnavailable)
		}))

		sub := &Subscription{Endpoint: "e"}
		topic := &Topic{ARN: "t", endpoints: map[string]*Subscription{"e": sub}}
		dlq := laneCall("dlq", "dead")
		fill := []delivery{
			{sub: sub, call: laneCall("dlq", "fill")},
			{sub: sub, call: laneCall("dlq", "fill")},
			{sub: sub, call: laneCall("dlq", "fill")},
		}

		var waiting sync.WaitGroup

		// The third waits for room until Close.
		waiting.Go(func() { p.lanes.dispatch(p.queue(topic, fill)) })

		require.Eventually(t, func() bool {
			p.lanes.mu.Lock()
			defer p.lanes.mu.Unlock()

			return p.lanes.queued == 3
		}, 10*time.Second, time.Millisecond)

		p.lanes.dispatch(p.queue(topic, []delivery{{sub: sub, call: laneCall("origin", "fail"), dlq: &dlq}}))

		require.Eventually(t, func() bool {
			p.lanes.mu.Lock()
			defer p.lanes.mu.Unlock()

			origin := p.lanes.byKey["origin"]

			return origin != nil && origin.held != nil
		}, 10*time.Second, time.Millisecond, "the origin lane never waited for the dead-letter queue")

		p.Close(t.Context())
		waiting.Wait()

		p.lanes.mu.Lock()
		assert.Empty(t, p.lanes.byKey, "iteration %d", iter)
		assert.Zero(t, p.lanes.queued)
		p.lanes.mu.Unlock()
		assert.Equal(t, int64(4), p.Dropped())
	}
}

// Test_lanes_parkedLanesTakeTheirTurn: 5,000 destinations of five deliveries
// each, more than the client holds. A lane the client refused is parked and
// takes its turn as answers free room, ahead of a busy lane's next delivery:
// every lane's first delivery is made within the first two deliveries per
// lane, where a lane that restarts itself first would keep the parked lanes
// waiting until the busy ones are empty.
func Test_lanes_parkedLanesTakeTheirTurn(t *testing.T) {
	const destinations, each = 5000, 5

	p := New(nil)
	t.Cleanup(func() { p.Close(t.Context()) })

	release := make(chan struct{})

	var (
		served    atomic.Int64
		mu        sync.Mutex
		lastFirst int64
	)

	p.Bind(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release

		body, _ := io.ReadAll(r.Body)
		n := served.Add(1)

		if bytes.Equal(body, []byte("0")) {
			mu.Lock()
			lastFirst = max(lastFirst, n)
			mu.Unlock()
		}

		w.WriteHeader(http.StatusOK)
	}))

	sub := &Subscription{Endpoint: "e"}
	topic := &Topic{ARN: "t", endpoints: map[string]*Subscription{"e": sub}}
	deliveries := make([]delivery, 0, destinations*each)

	for i := range each {
		for d := range destinations {
			deliveries = append(deliveries, delivery{sub: sub, call: laneCall("q"+strconv.Itoa(d), strconv.Itoa(i))})
		}
	}

	p.lanes.dispatch(p.queue(topic, deliveries))
	close(release)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	p.lanes.awaitIdle(ctx)
	require.NoError(t, ctx.Err())
	assert.Equal(t, int64(destinations*each), served.Load())

	mu.Lock()
	defer mu.Unlock()

	assert.LessOrEqual(t, lastFirst, int64(2*destinations),
		"the last lane's first delivery waited behind the busy lanes")
}

// panicError is an error whose message cannot be read: answered reads it when
// the transport's answer is an error, so the answer panics.
type panicError struct{}

func (panicError) Error() string { panic("answered: injected panic") }

// Test_lanes_answeredPanicKeepsLaneGoing: a panic in answered, on the client's
// worker, for one delivery. The panic still reaches the worker (which logs it
// and keeps serving), but the lane serves its next deliveries, nothing is
// dropped, every delivery is made once, and Hub.Drain returns before its
// deadline instead of waiting for deliveries stuck behind a lane that never
// went on.
func Test_lanes_answeredPanicKeepsLaneGoing(t *testing.T) {
	const n = 3

	p := New(nil)

	var served atomic.Int64

	hub := inproc.NewHub(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	p.Bind(hub)
	t.Cleanup(func() { p.Close(t.Context()) })

	topic, deliveries := laneFixture(n, 1)
	jobs := p.queue(topic, deliveries)

	// The lane's first answer is replaced by one whose error panics, the way
	// the client's worker meets a panicking done. The test stands in for the
	// worker's recover and keeps what it recovered.
	recovered := make(chan any, 1)

	p.lanes.mu.Lock()

	answer := jobs[0].lane.done
	first := true

	jobs[0].lane.done = func(resp inproc.Response, err error) {
		p.lanes.mu.Lock()
		panicking := first
		first = false
		p.lanes.mu.Unlock()

		if !panicking {
			answer(resp, err)

			return
		}

		defer func() { recovered <- recover() }()

		answer(resp, panicError{})
	}

	p.lanes.mu.Unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	drained := make(chan struct{})

	go func() {
		defer close(drained)

		hub.Drain(ctx)
	}()

	p.lanes.dispatch(jobs)
	<-drained

	require.NoError(t, ctx.Err(), "Drain waited for its deadline: the lane stalled after the panic")
	assert.Equal(t, int64(n), served.Load(), "the lane served every delivery")
	assert.Equal(t, "answered: injected panic", <-recovered, "the panic reached the worker")
	assert.Zero(t, p.Dropped())

	p.lanes.mu.Lock()
	defer p.lanes.mu.Unlock()

	assert.Zero(t, p.lanes.queued, "deliveries left in the lanes")
	assert.Empty(t, p.lanes.byKey, "the lane was not forgotten")
}
