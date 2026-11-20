package gateway_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memListener is a net.Listener with no socket: each connection is a net.Pipe.
// A goroutine blocked in Accept or on such a connection is durably blocked, so
// a test that serves over it can run in a testing/synctest bubble and have the
// drain's deadline on the fake clock. A real socket would stop that clock.
type memListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newMemListener() *memListener {
	return &memListener{conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *memListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *memListener) Close() error {
	l.once.Do(func() { close(l.closed) })

	return nil
}

func (*memListener) Addr() net.Addr { return memAddr{} }

// client is an HTTP client whose every connection goes to the listener, and
// that keeps none open after a response, so a bubble can end.
func (l *memListener) client() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			client, server := net.Pipe()

			select {
			case l.conns <- server:
				return client, nil
			case <-l.closed:
				_ = client.Close()
				_ = server.Close()

				return nil, net.ErrClosed
			case <-ctx.Done():
				_ = client.Close()
				_ = server.Close()

				return nil, fmt.Errorf("dial: %w", context.Cause(ctx))
			}
		},
		DisableKeepAlives: true,
	}}
}

type memAddr struct{}

func (memAddr) Network() string { return "mem" }
func (memAddr) String() string  { return "mem" }

// Test_Gateway_drainByIsTheDrainsDeadline: the time DrainBy names is the instant
// at which Serve stops waiting for a request that never ends, and no other: not
// the default drain of five seconds that a DrainBy ignored would leave, and not
// the request's own end.
//
// It runs on synctest's clock, over an in-memory listener, so the instant is
// exact and load cannot move it. Test_Gateway_drainBy runs the same cut on a
// real socket, where only its lower bound can be asserted.
//
// A DrainBy taken as no limit fails by assertion too, at a guard on the fake
// clock: Serve then drains for ever, and the shutdown's own polling keeps the
// fake clock moving, so without the guard the test would run to go test's
// timeout and take the results of the rest of the package with it.
func Test_Gateway_drainByIsTheDrainsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			// Far from the default drain, so a DrainBy that is ignored cannot land on it.
			drainWait = 150 * time.Millisecond
			// guard is past the default drain, so a DrainBy that is ignored still ends
			// the drain and fails on the instant, not on the guard.
			guard = time.Minute
		)

		started := make(chan struct{})
		release := make(chan struct{})

		// The request ends when the drain cuts it, or when the test lets it go after a
		// drain that never ended, so that the bubble can end with the failure.
		hang := func(_ http.ResponseWriter, rc *request.Context) {
			close(started)

			select {
			case <-rc.Request.Context().Done():
			case <-release:
			}
		}

		g := newGateway(t, newSettings(), []service.Registration{sqsRegistration(hang)})
		mem := newMemListener()
		done := make(chan error, 1)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		go func() { done <- g.Serve(ctx, mem) }()

		failed := make(chan error, 1)

		go func() {
			_, _, err := fetch(t.Context(), mem.client(), "http://mem/")
			failed <- err
		}()

		<-started

		begin := time.Now()

		g.DrainBy(begin.Add(drainWait))
		cancel()

		select {
		case err := <-done:
			require.NoError(t, err, "cutting a request at the deadline is not an error")
		case <-time.After(guard):
			// Serve ends once the request has, and the bubble must not end before it.
			close(release)
			err := <-done

			require.FailNow(t, "the drain did not end",
				"still draining %s after DrainBy named %s (Serve returned %v once the request was released)",
				guard, drainWait, err)
		}

		assert.Equal(t, drainWait, time.Since(begin), "the drain ended at DrainBy")
		require.Error(t, <-failed, "the request in flight was cut")
	})
}
