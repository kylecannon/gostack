package sns

import (
	"context"
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

// Test_lanes_closeRacesPublish queues 3,000 deliveries over 32 lanes while
// Close runs, at a different moment each time: every delivery is accounted for
// once, made or dropped, and no count goes below zero. Run it with
// -count=200.
func Test_lanes_closeRacesPublish(t *testing.T) {
	const n, lanes = 3000, 32

	for iter := range 20 {
		p := New(nil)

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

	p := New(nil)
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
