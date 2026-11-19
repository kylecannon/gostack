package gateway_test

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/inproc"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveWithDrain starts Serve on a loopback port with drain as its DrainCalls
// function, and returns the address and a function that ends Serve and returns
// its error.
func serveWithDrain(
	t *testing.T, g *gateway.Gateway, drain func(ctx context.Context),
) (addr string, stop func() error) {
	t.Helper()

	g.DrainCalls(drain)

	var config net.ListenConfig

	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- g.Serve(ctx, listener) }()

	var once atomic.Bool

	stop = func() error {
		if !once.CompareAndSwap(false, true) {
			return nil
		}

		cancel()

		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			return context.DeadlineExceeded
		}
	}

	t.Cleanup(func() { _ = stop() })

	return listener.Addr().String(), stop
}

// Test_Gateway_drainCalls: once the requests from outside have drained, Serve
// still serves the calls services make to each other until its DrainCalls
// function returns; nothing from outside gets in meanwhile; afterwards every
// call is refused and no service handler runs.
func Test_Gateway_drainCalls(t *testing.T) {
	var reached atomic.Int32

	g := newGateway(t, newSettings(), []service.Registration{
		sqsRegistration(func(w http.ResponseWriter, rc *request.Context) {
			reached.Add(1)
			okServe(w, rc)
		}),
	})

	callsBy := time.Now().Add(time.Minute)
	g.DrainBy(time.Now().Add(time.Minute), callsBy)

	type seen struct {
		dialErr  error
		external int
		internal int
		body     string
		deadline time.Time
	}

	during := make(chan seen, 1)
	external, internal := sendMessage(t), sendMessage(t)
	release := make(chan struct{})

	var addr string

	addr, stop := serveWithDrain(t, g, func(ctx context.Context) {
		var s seen

		var dialer net.Dialer

		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
		}

		s.dialErr = err

		resp, _ := call(t, g.Handler(), external)
		s.external = resp.StatusCode

		resp, s.body = call(t, g.InternalHandler(), internal)
		s.internal = resp.StatusCode

		s.deadline, _ = ctx.Deadline()
		during <- s

		<-release
	})

	stopped := make(chan error, 1)

	go func() { stopped <- stop() }()

	got := <-during

	require.Error(t, got.dialErr, "the listener is closed before the in-process calls drain")
	assert.Equal(t, http.StatusServiceUnavailable, got.external, "a request from outside is refused")
	assert.Equal(t, http.StatusOK, got.internal, "an internal call is served")
	assert.Equal(t, "ok", got.body)
	assert.True(t, got.deadline.Equal(callsBy), "the drain ends at the calls deadline")
	assert.Equal(t, int32(1), reached.Load())

	close(release)
	require.NoError(t, <-stopped)

	resp, _ := call(t, g.InternalHandler(), sendMessage(t))
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "after the drain, an internal call is refused")
	assert.Equal(t, int32(1), reached.Load(), "and never reaches the service")
}

// Test_Gateway_drainCallsDeliversAQueuedCall is the in-process half of an S3
// notification on a clean stop: a call queued before the shutdown and held
// until it has begun is still served, and Serve returns only after it.
func Test_Gateway_drainCallsDeliversAQueuedCall(t *testing.T) {
	var (
		status  atomic.Int32
		reached atomic.Int32
	)

	entered := make(chan struct{})
	hold := make(chan struct{})
	begun := make(chan struct{})

	reg := sqsRegistration(func(w http.ResponseWriter, rc *request.Context) {
		reached.Add(1)
		okServe(w, rc)
	})
	reg.Shutdown = func() { close(begun) }

	g := newGateway(t, newSettings(), []service.Registration{reg})

	hub := inproc.NewHub(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-hold
		g.InternalHandler().ServeHTTP(w, r)
	}))
	client := inproc.New()
	client.Bind(hub)

	t.Cleanup(client.Close)

	g.DrainBy(time.Now().Add(time.Minute), time.Now().Add(time.Minute))

	_, stop := serveWithDrain(t, g, func(ctx context.Context) { hub.Drain(ctx) })

	require.True(t, client.Enqueue(inproc.Call{
		Account: "123456789012", Region: usEast1, Service: nameSQS, Target: targetSendMessage, Body: []byte(`{}`),
	}, func(resp inproc.Response, err error) {
		if err == nil {
			status.Store(int32(resp.Status))
		}
	}))
	<-entered

	stopped := make(chan error, 1)

	go func() { stopped <- stop() }()

	<-begun

	resp, _ := call(t, g.Handler(), sendMessage(t))
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "the shutdown has begun")

	select {
	case err := <-stopped:
		t.Fatalf("Serve returned with a call still queued: %v", err)
	default:
	}

	close(hold)
	require.NoError(t, <-stopped)

	assert.Equal(t, int32(http.StatusOK), status.Load(), "the queued call was served during the drain")
	assert.Equal(t, int32(1), reached.Load())
	assert.Zero(t, hub.Cut())
}
