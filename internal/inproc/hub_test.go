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

			assert.Equal(t, 100*time.Millisecond, time.Since(begin), "Drain returned at its deadline")
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
