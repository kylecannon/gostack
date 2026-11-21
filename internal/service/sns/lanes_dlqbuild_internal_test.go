package sns

import (
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/inproc"
	"github.com/kylecannon/gostack/internal/service/sns/envelope"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failingEndpoint is an HTTP endpoint that fails every delivery.
func failingEndpoint(t *testing.T) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	return srv.URL
}

// lockFreesSoon reports whether mu can be taken within 2 s. A lock the caller
// holds never frees until the build returns, while another goroutine (a lane
// finishing, a dispatch) releases it within microseconds, so one TryLock would
// fail at random where this fails only for a caller-held lock.
func lockFreesSoon(mu *sync.Mutex) bool {
	deadline := time.Now().Add(2 * time.Second)

	for {
		if mu.TryLock() {
			mu.Unlock()

			return true
		}

		if time.Now().After(deadline) {
			return false
		}

		runtime.Gosched()
	}
}

// httpJob is an HTTP delivery to endpoint whose dead-letter call is dlq.
func httpJob(endpoint string, dlq func() inproc.Call) *job {
	return &job{
		call: endpointCall(endpoint),
		post: &post{
			url: endpoint, kind: envelope.Notification, messageID: "m", topicARN: "t", subscription: "s",
			body: []byte("{}"),
		},
		dlq: dlq,
	}
}

// queueJob adds j to its lane and starts it, as a publish does.
func queueJob(t *testing.T, p *Provider, j *job) {
	t.Helper()

	ls := &p.lanes

	ls.mu.Lock()
	added := ls.add(j)
	ls.mu.Unlock()

	require.True(t, added)
	ls.dispatch([]*job{j})
	ls.awaitIdle(t.Context())
}

// Test_lanes_httpDeadLetterBuiltOutsideLock: an HTTP delivery that fails has
// its dead-letter call built with the lanes lock free, so a call that costs
// 1.5 ms and 1.8 MB for a 256 KiB message holds up no publish (SQ-394); the
// call goes to the dead-letter queue as built.
func Test_lanes_httpDeadLetterBuiltOutsideLock(t *testing.T) {
	var (
		built    atomic.Int64
		lockFree atomic.Bool
		dead     atomic.Int64
	)

	p := New(nil, nil)
	t.Cleanup(func() { p.Close(t.Context()) })

	p.Bind(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) == `"dead"` {
			dead.Add(1)
		}

		w.WriteHeader(http.StatusOK)
	}))

	j := httpJob(failingEndpoint(t), func() inproc.Call {
		built.Add(1)

		lockFree.Store(lockFreesSoon(&p.lanes.mu))

		return laneCall("dlq", `"dead"`)
	})

	queueJob(t, p, j)

	assert.Equal(t, int64(1), built.Load(), "the dead-letter call is built once, when the delivery fails")
	assert.True(t, lockFree.Load(), "the dead-letter call was built with the lanes lock held")
	assert.Equal(t, int64(1), dead.Load(), "the dead letter reached its queue as built")
	assert.Zero(t, p.Dropped())
}

// Test_lanes_queueDeadLetterBuiltOutsideLock: a queue's delivery that fails has
// its dead-letter call built with the lanes lock free too (the splice of a
// 256 KiB message took 35 to 70 ms of the lock in SQ-392's burst), the call
// goes to the dead-letter queue as built, and one that panics drops the
// delivery with the lane going on.
func Test_lanes_queueDeadLetterBuiltOutsideLock(t *testing.T) {
	var (
		built    atomic.Int64
		lockFree atomic.Bool
		dead     atomic.Int64
	)

	p := New(nil, nil)
	t.Cleanup(func() { p.Close(t.Context()) })

	p.Bind(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) == `"dead"` {
			dead.Add(1)
			w.WriteHeader(http.StatusOK)

			return
		}

		w.WriteHeader(http.StatusInternalServerError)
	}))

	queueJob(t, p, &job{call: laneCall("q", `"primary"`), dlq: func() inproc.Call {
		built.Add(1)

		lockFree.Store(lockFreesSoon(&p.lanes.mu))

		return laneCall("dlq", `"dead"`)
	}})

	assert.Equal(t, int64(1), built.Load(), "the dead-letter call is built once, when the delivery fails")
	assert.True(t, lockFree.Load(), "the dead-letter call was built with the lanes lock held")
	assert.Equal(t, int64(1), dead.Load(), "the dead letter reached its queue as built")
	assert.Zero(t, p.Dropped())

	for range 3 {
		queueJob(t, p, &job{call: laneCall("q", `"primary"`), dlq: func() inproc.Call { panic("cannot build") }})
	}

	assert.Equal(t, int64(3), p.Dropped(), "each delivery whose dead-letter call panicked is dropped, the lane went on")
}

// Test_lanes_httpDeadLetterNotBuiltWhenDelivered: a delivery that succeeds
// builds nothing, and one without a dead-letter queue is dropped.
func Test_lanes_httpDeadLetterNotBuiltWhenDelivered(t *testing.T) {
	var built atomic.Int64

	p := New(nil, nil)
	t.Cleanup(func() { p.Close(t.Context()) })

	p.Bind(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ok.Close)

	queueJob(t, p, httpJob(ok.URL, func() inproc.Call {
		built.Add(1)

		return laneCall("dlq", "{}")
	}))
	assert.Zero(t, built.Load(), "a delivery that succeeded built a dead-letter call")
	assert.Zero(t, p.Dropped())

	queueJob(t, p, httpJob(failingEndpoint(t), nil))
	assert.Equal(t, int64(1), p.Dropped(), "a failed delivery without a dead-letter queue is dropped")
}

// Test_lanes_httpDeadLetterPanicDropsDelivery: a dead-letter call that panics
// while it is built drops its delivery, and the lane goes on with the next
// delivery, as it does for a send that panics.
func Test_lanes_httpDeadLetterPanicDropsDelivery(t *testing.T) {
	p := New(nil, nil)
	t.Cleanup(func() { p.Close(t.Context()) })

	p.Bind(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	endpoint := failingEndpoint(t)

	for range 3 {
		queueJob(t, p, httpJob(endpoint, func() inproc.Call { panic("cannot build") }))
	}

	assert.Equal(t, int64(3), p.Dropped(), "each delivery whose dead-letter call panicked is dropped, the lane went on")
}
