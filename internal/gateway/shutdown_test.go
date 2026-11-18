package gateway_test

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_Gateway_shutdownHooks follows LocalStack's shutdown: the gateway refuses
// new AWS calls first (RuntimeShutdownHandler, aws/handlers/internal.py), then
// the services are told to stop (on_before_stop), and each is told once.
func Test_Gateway_shutdownHooks(t *testing.T) {
	var (
		g      *gateway.Gateway
		calls  atomic.Int32
		status atomic.Int32
	)

	reg := sqsRegistration(okServe)
	reg.Shutdown = func() {
		calls.Add(1)

		// A call made while the hook runs is already refused.
		resp, _ := call(t, g.Handler(), sendMessage(t))
		status.Store(int32(resp.StatusCode))
	}

	g = newGateway(t, newSettings(), []service.Registration{reg, stsRegistration(okServe)})

	g.BeginShutdown()
	g.BeginShutdown()

	assert.Equal(t, int32(1), calls.Load(), "the hook runs once, however often shutdown begins")
	assert.Equal(t, int32(http.StatusServiceUnavailable), status.Load(),
		"new AWS calls are refused before the hook runs")
}

// Test_Gateway_shutdownHooksRunWhenServeStops covers the path that has no
// caller of BeginShutdown: the context of Serve ends.
func Test_Gateway_shutdownHooksRunWhenServeStops(t *testing.T) {
	var calls atomic.Int32

	reg := sqsRegistration(okServe)
	reg.Shutdown = func() { calls.Add(1) }

	s := serve(t, newSettings(), reg)

	s.cancel()

	require.Eventually(t, func() bool { return calls.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
	require.NoError(t, s.stop(t))
	assert.Equal(t, int32(1), calls.Load())
}
