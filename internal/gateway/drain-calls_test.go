package gateway_test

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_Gateway_EndShutdown: once Serve has drained the requests from outside
// and returned, its listener is closed and the gateway still serves the calls
// services make to each other, while it refuses a request that is not one.
// After EndShutdown every call is refused and no service handler runs.
func Test_Gateway_EndShutdown(t *testing.T) {
	var reached atomic.Int32

	g := newGateway(t, newSettings(), []service.Registration{
		sqsRegistration(func(w http.ResponseWriter, rc *request.Context) {
			reached.Add(1)
			okServe(w, rc)
		}),
	})

	var config net.ListenConfig

	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- g.Serve(ctx, listener) }()

	cancel()

	select {
	case served := <-done:
		require.NoError(t, served)
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}

	var dialer net.Dialer

	conn, err := dialer.DialContext(t.Context(), "tcp", listener.Addr().String())
	if err == nil {
		_ = conn.Close()
	}

	require.Error(t, err, "the listener is closed")

	resp, _ := call(t, g.Handler(), sendMessage(t))
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "a request that is not an internal call is refused")

	resp, body := call(t, g.InternalHandler(), sendMessage(t))
	assert.Equal(t, http.StatusOK, resp.StatusCode, "an internal call is served")
	assert.Equal(t, "ok", body)
	assert.Equal(t, int32(1), reached.Load())

	g.EndShutdown()

	resp, _ = call(t, g.InternalHandler(), sendMessage(t))
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "after EndShutdown, an internal call is refused")
	assert.Equal(t, int32(1), reached.Load(), "and never reaches the service")
}
