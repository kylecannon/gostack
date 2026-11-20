package inproc_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kylecannon/gostack/internal/inproc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureLog sends slog's default logger to a buffer for the test.
func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()

	buf := &lockedBuffer{}
	previous := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	return buf
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p) //nolint:wrapcheck // bytes.Buffer.Write never fails
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func Test_Hub(t *testing.T) {
	t.Run("Drain waits for every queued call, and for the calls they queue", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			release := make(chan struct{})

			var served atomic.Int32

			second := inproc.New()

			hub := inproc.NewHub(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				<-release

				if served.Add(1) == 1 {
					// A call served queues one more, on the other client.
					assert.True(t, second.Enqueue(sqsCall("follow-up"), nil))
				}
			}))

			first := inproc.New()
			first.Bind(hub)
			second.Bind(hub)

			for i := range 3 {
				require.True(t, first.Enqueue(laneCall(i, i), nil))
			}

			drained := make(chan int, 1)

			go func() { drained <- hub.Drain(context.Background()) }()

			synctest.Wait()

			select {
			case <-drained:
				t.Fatal("Drain returned with calls still queued")
			default:
			}

			close(release)

			assert.Zero(t, <-drained, "nothing was cut")
			assert.Equal(t, int32(4), served.Load(), "the three queued calls and the one they queued were served")
			assert.Zero(t, hub.Cut())

			first.Close()
			second.Close()
		})
	})

	t.Run("Drain cuts a thousand slow calls at its deadline and logs how many", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			log := captureLog(t)

			stuck := make(chan struct{})

			var started atomic.Int32

			hub := inproc.NewHub(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				started.Add(1)
				// A slow destination that ignores its request's context.
				<-stuck
			}))

			client := inproc.New()
			client.Bind(hub)

			answered := atomic.Int32{}

			for i := range 1000 {
				require.True(t, client.Enqueue(laneCall(i, i), func(inproc.Response, error) { answered.Add(1) }))
			}

			synctest.Wait()

			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()

			begin := time.Now()
			cut := hub.Drain(ctx)

			assert.Equal(t, 120*time.Millisecond, time.Since(begin),
				"Drain cut at its deadline and waited 20 ms for the calls it cancelled, which never return")
			assert.Equal(t, 1000, cut, "the calls running and the calls queued were cut")
			assert.Equal(t, 1000, hub.Cut())
			assert.Equal(t, int32(inproc.MaxInFlight), started.Load(), "only the calls running reached the handler")
			assert.Contains(t, log.String(), "in-process calls cut at shutdown")
			assert.Contains(t, log.String(), "count=1000")

			assert.False(t, client.Enqueue(sqsCall("after the drain"), nil), "the queue is shut")
			assert.Equal(t, 1001, hub.Cut(), "a call refused after the drain is counted")

			close(stuck)
			client.Close()

			assert.Equal(t, int32(inproc.MaxInFlight), started.Load(), "no queued call was served after the cut")
			assert.Equal(t, int32(inproc.MaxInFlight), answered.Load(), "only the calls running were answered")
		})
	})

	t.Run("a call queued after the drain never reaches the handler", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var reached atomic.Int32

			hub := inproc.NewHub(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
			client := inproc.New()
			client.Bind(hub)

			assert.Zero(t, hub.Drain(context.Background()), "an idle hub has nothing to cut")

			for i := range 3 {
				assert.False(t, client.Enqueue(sqsCall(strconv.Itoa(i)), nil))
			}

			synctest.Wait()
			client.Close()

			assert.Zero(t, reached.Load(), "not delivered")
			assert.Equal(t, 3, hub.Cut(), "counted")
		})
	})

	t.Run("a client binds to a hub once", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			release := make(chan struct{})
			hub := inproc.NewHub(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
			client := inproc.New()

			client.Bind(hub)
			client.Bind(hub)
			require.True(t, client.Enqueue(sqsCall("one"), nil))
			synctest.Wait()

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			assert.Equal(t, 1, hub.Drain(ctx), "the call running is cut once")

			close(release)
			client.Close()
		})
	})
}

// Test_Hub_DrainWaitsForAChainOfClients is the review's chain (SQ-294, S2):
// clients bound a, b, c, and a call on c queues one on b, whose handler queues
// one on a. Drain has no deadline, so it may return only once the chain has
// ended, and then it has nothing to cut. Checking each client in turn missed
// the call on a about once in 1,800 runs.
func Test_Hub_DrainWaitsForAChainOfClients(t *testing.T) {
	const iterations = 20000

	early := 0

	var spun atomic.Int64

	for i := range iterations {
		var a, b *inproc.Client

		var servedA atomic.Int32

		hub := inproc.NewHub(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/c":
				b.Enqueue(laneCall(0, i), nil)
			case "/0":
				// A handler of varying length, so b ends at varying points of the
				// drain's wait.
				for range i % 97 {
					spun.Add(1)
				}

				a.Enqueue(laneCall(1, i), nil)
			default:
				servedA.Add(1)
			}
		}))

		a, b = inproc.New(), inproc.New()
		c := inproc.New()

		a.Bind(hub)
		b.Bind(hub)
		c.Bind(hub)

		call := sqsCall("chain")
		call.Path = "/c"
		require.True(t, c.Enqueue(call, nil))

		if hub.Drain(context.Background()) != 0 || servedA.Load() != 1 {
			early++
		}

		a.Close()
		b.Close()
		c.Close()
	}

	assert.Zero(t, early, "Drain returned before the chain ended in %d of %d runs", early, iterations)
}

// Test_Hub_DrainCountsOnlyTheCallsItCut: a call running when Drain cuts, whose
// handler still answers it, was served and is not counted as cut (SQ-294, N3).
func Test_Hub_DrainCountsOnlyTheCallsItCut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		log := captureLog(t)

		hub := inproc.NewHub(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The cut cancels the call; the handler finishes it all the same.
			<-r.Context().Done()
			w.WriteHeader(http.StatusOK)
		}))
		client := inproc.New()
		client.Bind(hub)

		var status atomic.Int32

		require.True(t, client.Enqueue(sqsCall("finishing"), func(resp inproc.Response, err error) {
			if err == nil {
				status.Store(int32(resp.Status))
			}
		}))
		synctest.Wait()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		assert.Zero(t, hub.Drain(ctx), "the call was answered")
		assert.Zero(t, hub.Cut())
		assert.Equal(t, int32(http.StatusOK), status.Load())
		assert.NotContains(t, log.String(), "in-process calls cut at shutdown")

		client.Close()
	})
}

// Test_Client_Hold: work a client holds counts in its hub's pending count until
// released, so Drain waits for it, and a second release counts nothing.
func Test_Client_Hold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hub := inproc.NewHub(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		client := inproc.New()
		client.Bind(hub)

		release := client.Hold()
		other := client.Hold()

		other()
		other()

		drained := make(chan struct{})

		go func() {
			defer close(drained)

			hub.Drain(context.Background())
		}()

		synctest.Wait()

		select {
		case <-drained:
			t.Fatal("Drain returned while the client held work")
		default:
		}

		// The held work queues its call, then lets go.
		require.True(t, client.Enqueue(sqsCall("held"), nil))
		release()
		<-drained

		assert.Zero(t, hub.Cut())
		assert.NotNil(t, inproc.New().Hold(), "a client with no hub holds nothing")

		client.Close()
	})
}
