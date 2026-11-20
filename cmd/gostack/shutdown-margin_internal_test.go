package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kylecannon/gostack/internal/config"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/lifecycle"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_serve_aBlockedShutdownCostsWorkAndNoMore is where the margin before the
// supervisor's SIGKILL is asserted: a service shutdown function that never
// returns holds the shutdown only until lifecycle.Deadlines.Work, so the save
// starts at that exact instant and ends at lifecycle.Deadlines.Save, which is
// KillMargin before the kill. A shutdown that overran Work by any amount, or a
// save that started late, moves the instant.
//
// It runs on synctest's clock, over an in-memory listener, so load cannot move
// the instants. The shutdown hooks cannot join it: a hook is a real process,
// which stops the fake clock. Test_serve_savesBeforeTheKill runs them, and
// asserts order and lower bounds only. The shutdown function and the hooks run
// in one goroutine under the same workBy, so this test covers both.
//
// A defect that never ends the shutdown, or that never saves, fails here by
// assertion at a limit on the fake clock: every wait for a result goes through
// within, since a receive that nothing answers is a deadlock panic that ends the
// test binary, and with it the results of the rest of the package.
func Test_serve_aBlockedShutdownCostsWorkAndNoMore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		serveBlockedShutdown(t, func(stop context.CancelFunc, _ *memListener) { stop() }, assert.NoError)
	})
}

// Test_serve_aBlockedShutdownCostsWorkAndNoMoreWhenAListenerFails is the same
// margin for the other cause of a shutdown: a listener fails, the gateway
// starts the shutdown inside Serve, and serve finds it begun. A service
// shutdown function that Serve ran inline held Serve, and serve with it, until
// the supervisor's SIGKILL, with no save (SQ-379).
func Test_serve_aBlockedShutdownCostsWorkAndNoMoreWhenAListenerFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		serveBlockedShutdown(t, func(_ context.CancelFunc, mem *memListener) { _ = mem.Close() }, assert.Error)
	})
}

// serveBlockedShutdown runs serve over an in-memory listener with a service
// shutdown function that never returns, and has cause start the shutdown: the
// signal (the context ends) or the failure of the listener. servedErr is what
// serve's error must be for that cause.
func serveBlockedShutdown(
	t *testing.T, cause func(stop context.CancelFunc, mem *memListener), servedErr assert.ErrorAssertionFunc,
) {
	const (
		timeout = 2 * time.Second
		// The plan for 2 seconds: work ends at 1s, the in-process calls at 1.25s,
		// the save at 1.5s.
		workAt = time.Second
		saveBy = timeout - lifecycle.KillMargin
		// limit is how long the test waits for a result on the fake clock: past the
		// supervisor's SIGKILL, which no result may come after.
		limit = 2 * timeout
	)

	stuck := make(chan struct{})

	t.Cleanup(func() { close(stuck) })

	reg := service.Registration{
		Metadata: service.Metadata{
			Name: testService, SigningName: testService, EndpointPrefix: testService,
			Protocols: []protocol.Kind{protocol.KindQuery},
		},
		Serve:    func(http.ResponseWriter, *request.Context) {},
		Shutdown: func() { <-stuck },
	}

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{anyLoopbackPort}} },
		[]service.Registration{reg}, nil, nil,
	)
	require.NoError(t, err)

	ctx, stop := context.WithCancel(t.Context())
	defer stop()

	type saved struct {
		at     time.Time
		saveBy time.Time
	}

	record := func(to chan<- saved, saveCtx context.Context) {
		deadline, _ := saveCtx.Deadline()
		to <- saved{at: time.Now(), saveBy: deadline}
	}

	firsts, rests := make(chan saved, 1), make(chan saved, 1)
	proc := &lifecycle.Process{Hooks: lifecycle.NewHooks(t.TempDir()), Out: io.Discard}
	done := make(chan error, 1)
	mem := newMemListener()

	go func() {
		done <- serve(ctx, gw, []net.Listener{mem}, proc, timeout, nil, shutdownSave{
			first: func(saveCtx context.Context) { record(firsts, saveCtx) },
			rest:  func(saveCtx, _ context.Context) { record(rests, saveCtx) },
		})
	}()

	synctest.Wait()

	begin := time.Now()

	cause(stop, mem)

	first := within(t, firsts, limit, "the first save")
	rest := within(t, rests, limit, "the second save")

	servedErr(t, within(t, done, limit, "serve's return"))

	assert.Equal(t, workAt, first.at.Sub(begin), "the save started when Work ended, not later")
	assert.Equal(t, workAt, rest.at.Sub(begin), "the second save followed at once: no calls to drain")
	assert.Equal(t, saveBy, first.saveBy.Sub(begin), "the first save ends KillMargin before the kill")
	assert.Equal(t, saveBy, rest.saveBy.Sub(begin), "the second save ends KillMargin before the kill")
	assert.Equal(t, workAt, time.Since(begin), "serve returned when the save did")
}

// within is what ch delivers, or a failed test once limit has passed on the fake
// clock. A bubble test that receives without it turns a result that never comes
// into "all goroutines in bubble are blocked", a panic that ends the test binary
// and with it the results of every test after. The wait is on the bubble's own
// clock, so it costs no real time.
func within[T any](t *testing.T, ch <-chan T, limit time.Duration, what string) T {
	t.Helper()

	select {
	case got := <-ch:
		return got
	case <-time.After(limit):
		require.FailNow(t, what+" did not come", "nothing within %s on the fake clock", limit)
	}

	var none T

	return none
}

// notifyWriter reports, once, the first write that holds marker.
type notifyWriter struct {
	marker string
	seen   chan struct{}
	once   sync.Once
}

func (w *notifyWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), w.marker) {
		w.once.Do(func() { close(w.seen) })
	}

	return len(p), nil
}

// Test_start_endsWhenServeDoes: start does nothing after serve returns, so the
// process is gone when the save has finished, with the whole margin left before
// the supervisor's SIGKILL. A step added after serve in start (or in a defer
// of it) spends that margin, and the process exit tests, which run on real
// sockets and processes, assert only order and so cannot see it.
//
// It runs start itself on synctest's clock. The listener is a real socket, and
// a goroutine in its Accept is in IO wait, which the bubble counts as running:
// the fake clock does not move for as long as the listener is open, request or
// none. So nothing may wait on the clock until the listeners close, and the
// shutdown has nothing to drain: serve returns at the instant of the signal and
// so must start.
//
// What it catches is therefore a step after the listeners close: a sleep in start
// after serve returns is seen as the instant it ends (planted at 700 ms, SQ-373:
// 0s expected, 700ms actual). A wait before they close is not seen as an instant:
// a sleep in serve right after the signal, a service shutdown that waits on a
// timer, a drain that waits for DrainBy would hang the test until go test's
// timeout (10 minutes by default), and the results of the tests after it with it.
// A fake timer cannot guard that, since it never fires while the listener is
// open. The one wait that can be guarded is the start, against start returning an
// error before it listens, which is the select against done.
func Test_start_endsWhenServeDoes(t *testing.T) {
	t.Setenv("GATEWAY_LISTEN", anyLoopbackPort)
	t.Setenv("SHUTDOWN_TIMEOUT", "3")
	t.Setenv("PERSIST_BASE_DIR", "")

	cfg, err := config.Load()
	require.NoError(t, err)

	synctest.Test(t, func(t *testing.T) {
		// The channel belongs to the bubble, which is why it is made here.
		listening := &notifyWriter{marker: "gostack listening", seen: make(chan struct{})}
		previous := slog.Default()

		slog.SetDefault(slog.New(slog.NewTextHandler(listening, nil)))
		defer slog.SetDefault(previous)

		ctx, stop := context.WithCancel(context.Background())
		defer stop()

		done := make(chan error, 1)

		go func() { done <- start(ctx, cfg, io.Discard) }()

		select {
		case <-listening.seen:
		case err := <-done:
			require.FailNow(t, "start returned before it listened", "its error: %v", err)
		}

		// SIGTERM.
		begin := time.Now()

		stop()

		require.NoError(t, <-done)
		assert.Equal(t, time.Duration(0), time.Since(begin), "start returned when serve did")
	})
}
