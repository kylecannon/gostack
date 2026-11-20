package sns

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/inproc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_lanes_httpDeadLetterBuildsAreBounded: the dead-letter calls of failed
// HTTP deliveries are built at most cap(poster.building) at once, however many
// deliveries fail together, and every one of them is built in the end (SQ-394:
// a burst built one per delivery in flight, and builders on every processor
// left the publishes that waited for the lanes lock waiting for a processor).
// Proven to catch: a bound of more than the places, or none, which lets every
// failed delivery build at once. The wait that remains, for a build that
// should not have started, is a guard that can only miss a violation, never
// raise one.
func Test_lanes_httpDeadLetterBuildsAreBounded(t *testing.T) {
	var (
		entered, running, most atomic.Int64
		answered               atomic.Int64
	)

	p := New(nil)
	t.Cleanup(func() { p.Close(t.Context()) })

	p.Bind(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		answered.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	places := cap(p.lanes.poster.building)
	failing := places + 3 // each in its own lane, so each is in flight at once

	gate := make(chan struct{})
	ls := &p.lanes
	jobs := make([]*job, failing)

	for i := range jobs {
		jobs[i] = httpJob(srv.URL+"/"+strconv.Itoa(i), func() inproc.Call {
			entered.Add(1)

			now := running.Add(1)
			defer running.Add(-1)

			for {
				seen := most.Load()
				if now <= seen || most.CompareAndSwap(seen, now) {
					break
				}
			}

			<-gate

			return laneCall("dlq", `"dead"`)
		})

		ls.mu.Lock()
		added := ls.add(jobs[i])
		ls.mu.Unlock()

		require.True(t, added)
	}

	ls.dispatch(jobs)

	// A guard against a hang: every place fills and every send has failed, so
	// the deliveries past the places are waiting for one.
	require.Eventually(t, func() bool {
		return entered.Load() >= int64(places) && answered.Load() == int64(failing)
	}, 30*time.Second, time.Millisecond, "the builds did not fill the places")

	assert.Never(t, func() bool { return entered.Load() > int64(places) }, 100*time.Millisecond, time.Millisecond,
		"a build started with every place taken")

	close(gate)
	ls.awaitIdle(t.Context())

	assert.Equal(t, int64(failing), entered.Load(), "every failed delivery's dead-letter call was built")
	assert.LessOrEqual(t, most.Load(), int64(places), "more dead-letter calls were built at once than the places")
	assert.Zero(t, p.Dropped())
}
