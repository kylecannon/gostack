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

// laneFixture is a topic with one subscription, and n deliveries for it
// spread over lanes lanes.
func laneFixture(n, lanes int) (*Topic, []delivery) {
	sub := &Subscription{Endpoint: "arn:aws:sqs:us-east-1:000000000000:q"}
	topic := &Topic{ARN: "arn:aws:sns:us-east-1:000000000000:l", endpoints: map[string]*Subscription{sub.Endpoint: sub}}
	deliveries := make([]delivery, n)

	for i := range deliveries {
		deliveries[i] = delivery{sub: sub, call: inproc.Call{
			Service: sqsService, Target: sqsSendMessage, Lane: "q" + strconv.Itoa(i%lanes), Body: []byte("{}"),
		}}
	}

	return topic, deliveries
}

// Test_lanes_idleAcrossDeadLetter: a delivery fails and goes to its
// dead-letter queue. The lanes are never idle between the two, so Close and
// AwaitDeliveries wait for the dead-letter delivery too.
func Test_lanes_idleAcrossDeadLetter(t *testing.T) {
	primaryGo, deadEntered, deadGo := make(chan struct{}), make(chan struct{}), make(chan struct{})

	p := New(nil, nil)
	t.Cleanup(func() { p.Close(t.Context()) })

	// The calls held are let go before Close, when the test fails first.
	var letGo sync.Once

	release := func() { letGo.Do(func() { close(deadGo) }) }
	t.Cleanup(release)

	p.Bind(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte("primary")) {
			<-primaryGo
			w.WriteHeader(http.StatusInternalServerError)

			return
		}

		close(deadEntered)
		<-deadGo
		w.WriteHeader(http.StatusOK)
	}))

	topic, deliveries := laneFixture(1, 1)
	deliveries[0].call.Body = []byte(`"primary"`)
	deliveries[0].dlq = func() inproc.Call {
		return inproc.Call{Service: sqsService, Target: sqsSendMessage, Lane: "dlq", Body: []byte(`"dead"`)}
	}

	p.lanes.dispatch(p.queue(topic, deliveries))

	p.lanes.mu.Lock()
	idle := p.lanes.idle
	p.lanes.mu.Unlock()

	require.NotNil(t, idle)
	close(primaryGo)

	select {
	case <-deadEntered:
	case <-time.After(time.Minute):
		t.Fatal("the dead-letter delivery was not sent")
	}

	select {
	case <-idle:
		t.Fatal("the lanes signalled idle with the dead-letter delivery in flight")
	default:
	}

	release()
	<-idle
	assert.Zero(t, p.Dropped())
}

// Test_lanes_closeRacesPublish queues 3,000 deliveries over 32 lanes while
// Close runs, at a different moment each time: every delivery is accounted for
// once, made or dropped, and no count goes below zero. Run it with
// -count=200.
func Test_lanes_closeRacesPublish(t *testing.T) {
	const n, lanes = 3000, 32

	for iter := range 20 {
		p := New(nil, nil)

		var served atomic.Int64

		p.Bind(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			served.Add(1)
			w.WriteHeader(http.StatusOK)
		}))

		topic, deliveries := laneFixture(n, lanes)

		var publishing sync.WaitGroup

		publishing.Go(func() { p.lanes.dispatch(p.queue(topic, deliveries)) })

		time.Sleep(time.Duration(iter) * 20 * time.Microsecond)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		p.Close(ctx)
		publishing.Wait()

		require.Equal(t, int64(n), served.Load()+p.Dropped(), "iteration %d: served %d, dropped %d",
			iter, served.Load(), p.Dropped())

		p.lanes.mu.Lock()
		assert.Zero(t, p.lanes.queued)
		assert.Zero(t, p.lanes.inflight)
		assert.Empty(t, p.lanes.byKey)
		p.lanes.mu.Unlock()
	}
}

// Test_lanes_parked fills the client's queue past inproc.MaxQueued with the
// heads of that many lanes: the lanes the client refuses are parked, and
// started again as answers free room, so every delivery is made.
func Test_lanes_parked(t *testing.T) {
	const n = inproc.MaxQueued + 500

	p := New(nil, nil)
	t.Cleanup(func() { p.Close(t.Context()) })

	var served atomic.Int64

	release := make(chan struct{})

	p.Bind(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		served.Add(1)
		w.WriteHeader(http.StatusOK)
	}))

	topic, deliveries := laneFixture(n, n)

	p.lanes.dispatch(p.queue(topic, deliveries))

	p.lanes.mu.Lock()
	parked := len(p.lanes.parked)
	p.lanes.mu.Unlock()

	assert.Positive(t, parked)
	close(release)

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	p.lanes.awaitIdle(ctx)
	require.NoError(t, ctx.Err())
	assert.Equal(t, int64(n), served.Load())
	assert.Zero(t, p.Dropped())
}
