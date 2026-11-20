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
func Test_serve_aBlockedShutdownCostsWorkAndNoMore(t *testing.T) {
	synctest.Test(t, serveBlockedShutdown)
}

func serveBlockedShutdown(t *testing.T) {
	const (
		timeout = 2 * time.Second
		// The plan for 2 seconds: work ends at 1s, the in-process calls at 1.25s,
		// the save at 1.5s.
		workAt = time.Second
		saveBy = timeout - lifecycle.KillMargin
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

	go func() {
		done <- serve(ctx, gw, []net.Listener{newMemListener()}, proc, timeout, nil, shutdownSave{
			first: func(saveCtx context.Context) { record(firsts, saveCtx) },
			rest:  func(saveCtx, _ context.Context) { record(rests, saveCtx) },
		})
	}()

	synctest.Wait()

	// SIGTERM.
	begin := time.Now()

	stop()

	first, rest := <-firsts, <-rests

	require.NoError(t, <-done)

	assert.Equal(t, workAt, first.at.Sub(begin), "the save started when Work ended, not later")
	assert.Equal(t, workAt, rest.at.Sub(begin), "the second save followed at once: no calls to drain")
	assert.Equal(t, saveBy, first.saveBy.Sub(begin), "the first save ends KillMargin before the kill")
	assert.Equal(t, saveBy, rest.saveBy.Sub(begin), "the second save ends KillMargin before the kill")
	assert.Equal(t, workAt, time.Since(begin), "serve returned when the save did")
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
// It runs start itself on synctest's clock. The listener is a real socket, so
// nothing may wait on the clock while a request is in flight, and none is: the
// shutdown has nothing to drain, so serve returns at the instant of the signal
// and so must start.
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

		<-listening.seen

		// SIGTERM.
		begin := time.Now()

		stop()

		require.NoError(t, <-done)
		assert.Equal(t, time.Duration(0), time.Since(begin), "start returned when serve did")
	})
}
