package gateway_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
)

// returnsWithin reports whether f returns within limit. A BeginShutdown that
// waits on itself would otherwise hang the test until go test's own timeout and
// take every later result with it; here it fails by assertion.
func returnsWithin(f func(), limit time.Duration) bool {
	done := make(chan struct{})

	go func() {
		defer close(done)

		f()
	}()

	select {
	case <-done:
		return true
	case <-time.After(limit):
		return false
	}
}

// Test_Gateway_BeginShutdownAfterEndShutdown: a shutdown that EndShutdown ended
// before anything began has no functions to wait for, and BeginShutdown returns
// at once, running none.
func Test_Gateway_BeginShutdownAfterEndShutdown(t *testing.T) {
	var calls atomic.Int32

	reg := sqsRegistration(okServe)
	reg.Shutdown = func() { calls.Add(1) }

	g := newGateway(t, newSettings(), []service.Registration{reg})

	g.EndShutdown()

	assert.True(t, returnsWithin(g.BeginShutdown, 2*time.Second), "BeginShutdown after EndShutdown returns")
	assert.Equal(t, int32(0), calls.Load(), "the shutdown had ended, so no function runs")
}

// Test_Gateway_ShutdownFunctionBeginsShutdown: a Shutdown function that calls
// BeginShutdown is told the shutdown is under way, and returns. Waiting for the
// functions from inside one would wait for itself.
func Test_Gateway_ShutdownFunctionBeginsShutdown(t *testing.T) {
	var (
		g      *gateway.Gateway
		calls  atomic.Int32
		inside atomic.Bool
	)

	reg := sqsRegistration(okServe)
	reg.Shutdown = func() {
		calls.Add(1)
		inside.Store(returnsWithin(func() { g.BeginShutdown() }, 2*time.Second))
	}

	g = newGateway(t, newSettings(), []service.Registration{reg})

	assert.True(t, returnsWithin(g.BeginShutdown, 10*time.Second), "BeginShutdown returns once the function has")
	assert.True(t, inside.Load(), "BeginShutdown called by the function returns")
	assert.Equal(t, int32(1), calls.Load())
}

// Test_Gateway_Released: a caller after the first waits for the functions
// through Released, which closes when they have returned, and at once when the
// shutdown ended before it began.
func Test_Gateway_Released(t *testing.T) {
	t.Run("closes once the functions have returned, not before", func(t *testing.T) {
		release, entered := make(chan struct{}), make(chan struct{})

		reg := sqsRegistration(okServe)
		reg.Shutdown = func() {
			close(entered)
			<-release
		}

		g := newGateway(t, newSettings(), []service.Registration{reg})

		go g.BeginShutdown()

		<-entered

		assert.True(t, returnsWithin(g.BeginShutdown, 2*time.Second), "a later BeginShutdown does not wait")
		assert.False(t, returnsWithin(func() { <-g.Released() }, 50*time.Millisecond), "Released waits for the function")

		close(release)

		assert.True(t, returnsWithin(func() { <-g.Released() }, 2*time.Second), "Released closes when it has returned")
	})

	t.Run("closes at once when EndShutdown came first", func(t *testing.T) {
		g := newGateway(t, newSettings(), []service.Registration{sqsRegistration(okServe)})

		g.EndShutdown()
		g.EndShutdown()

		assert.True(t, returnsWithin(func() { <-g.Released() }, 2*time.Second), "Released is closed")
	})
}

// Test_Gateway_PanickingShutdownFunction: a Shutdown function that panics does
// not end the process or the functions after it, and BeginShutdown returns once
// they have all run.
func Test_Gateway_PanickingShutdownFunction(t *testing.T) {
	var after atomic.Int32

	panicking := sqsRegistration(okServe)
	panicking.Shutdown = func() { panic("a service's Shutdown function panics") }

	next := stsRegistration(okServe)
	next.Shutdown = func() { after.Add(1) }

	g := newGateway(t, newSettings(), []service.Registration{panicking, next})

	assert.True(t, returnsWithin(g.BeginShutdown, 10*time.Second), "BeginShutdown returns after a function panicked")
	assert.Equal(t, int32(1), after.Load(), "the function after the one that panicked ran")
}
