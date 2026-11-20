package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/lifecycle"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// anyLoopbackPort asks the kernel for a free port, so a test never takes 4566.
const anyLoopbackPort = "127.0.0.1:0"

// Test_serve_savesBeforeTheKill is the deadline test of a shutdown: whatever the
// hooks and the drain do, the save starts at lifecycle.Deadlines.Work and ends by
// lifecycle.Deadlines.Save, which is KillMargin before the supervisor's SIGKILL.
// A request that never ends is in flight in every case.
//
// It runs in real time, with a real socket and real hook processes, which a
// synctest bubble cannot move on. So it asserts order and lower bounds, and the
// waits for the save and for serve are guards against a hang with seconds to
// spare beyond the whole budget, not bounds on how long a step took. It does
// not assert KillMargin: a save that started late still passes it. The instants
// of the plan are asserted on the fake clock by
// Test_serve_aBlockedShutdownCostsWorkAndNoMore (the save starts at Work, and
// ends KillMargin before the kill, whatever the shutdown function does),
// Test_serve_listenerFailureKeepsToOneBudget and
// Test_serve_cutsQueuedCallsAtTheirDeadline.
func Test_serve_savesBeforeTheKill(t *testing.T) {
	const (
		timeout = 3 * time.Second
		// The plan for 3 seconds: work ends at 1.875s, the in-process calls at
		// 2.1875s, the save at 2.5s.
		saveBy = timeout - lifecycle.KillMargin
		// The drain keeps waiting, the stuck request in flight, until Work.
		drainFloor = 1500 * time.Millisecond
	)

	tests := []struct {
		name string
		// giveHook is the body of a shutdown hook, or "" for none.
		giveHook string
		// giveBlockedShutdown makes the service's own shutdown function never return.
		giveBlockedShutdown bool
	}{
		{name: "a request in flight and no hook"},
		{name: "a hook that runs far past the timeout", giveHook: "#!/bin/sh\nexec sleep 30\n"},
		{name: "a service shutdown function that never returns", giveBlockedShutdown: true},
		{
			name: "both", giveHook: "#!/bin/sh\nexec sleep 30\n", giveBlockedShutdown: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			started := make(chan struct{})
			stuck := make(chan struct{})

			t.Cleanup(func() { close(stuck) })

			reg := service.Registration{
				Metadata: service.Metadata{
					Name: testService, SigningName: testService, EndpointPrefix: testService,
					Protocols: []protocol.Kind{protocol.KindQuery},
				},
				Serve: func(_ http.ResponseWriter, rc *request.Context) {
					close(started)
					<-rc.Request.Context().Done()
				},
			}
			if tt.giveBlockedShutdown {
				reg.Shutdown = func() { <-stuck }
			}

			gw, err := gateway.New(
				func() gateway.Settings { return gateway.Settings{Listen: []string{anyLoopbackPort}} },
				[]service.Registration{reg}, nil, nil,
			)
			require.NoError(t, err)

			root := t.TempDir()
			if tt.giveHook != "" {
				hook := filepath.Join(root, "shutdown.d", "slow.sh")

				require.NoError(t, os.MkdirAll(filepath.Dir(hook), 0o750))
				// A hook must be executable.
				require.NoError(t, os.WriteFile(hook, []byte(tt.giveHook), 0o700))
			}

			ctx, stop := context.WithCancel(t.Context())
			defer stop()

			listeners, err := gw.Listen(ctx)
			require.NoError(t, err)

			type saved struct {
				at       time.Time
				deadline time.Time
				hasLimit bool
			}

			saves := make(chan saved, 1)
			proc := &lifecycle.Process{Hooks: lifecycle.NewHooks(root), Out: io.Discard}
			done := make(chan error, 1)

			go func() {
				done <- serve(ctx, gw, listeners, proc, timeout, nil, restOnly(func(saveCtx, _ context.Context) {
					deadline, hasLimit := saveCtx.Deadline()
					saves <- saved{at: time.Now(), deadline: deadline, hasLimit: hasLimit}
				}))
			}()

			go func() {
				req, reqErr := http.NewRequestWithContext(t.Context(), http.MethodPost,
					"http://"+listeners[0].Addr().String()+"/", strings.NewReader("Action=Hang"))
				if reqErr != nil {
					return
				}

				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=t/20240101/us-east-1/"+testService+
					"/aws4_request, SignedHeaders=host, Signature=abc")

				resp, doErr := http.DefaultClient.Do(req)
				if doErr == nil {
					_ = resp.Body.Close()
				}
			}()

			select {
			case <-started:
			case <-time.After(10 * time.Second):
				t.Fatal("the request never reached the service")
			}

			// SIGTERM.
			begin := time.Now()

			stop()

			var got saved

			select {
			case got = <-saves:
			case <-time.After(timeout + 5*time.Second):
				t.Fatal("the state was never saved")
			}

			assert.GreaterOrEqual(t, got.at.Sub(begin), drainFloor, "the drain used the time before the save reserve")
			require.True(t, got.hasLimit, "the save has a deadline")

			// The save's deadline is saveBy after the instant serve planned the
			// shutdown, which was after the signal and before the save began. That
			// is order only: it places the plan in a window that every late save
			// also satisfies. Test_serve_aBlockedShutdownCostsWorkAndNoMore asserts
			// the instants; no wall-clock bound is asserted here, because a loaded
			// host can run any step late.
			planned := got.deadline.Add(-saveBy)

			assert.False(t, planned.Before(begin), "the plan counts from the signal, not before it")
			assert.False(t, planned.After(got.at), "the plan was made before the save began")

			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("serve did not return")
			}
		})
	}
}

// Test_serve_withoutATimeoutHasNoDeadline: SHUTDOWN_TIMEOUT=0 is no limit, so the
// save has no deadline either.
func Test_serve_withoutATimeoutHasNoDeadline(t *testing.T) {
	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{anyLoopbackPort}} },
		nil, nil, nil,
	)
	require.NoError(t, err)

	ctx, stop := context.WithCancel(t.Context())
	defer stop()

	listeners, err := gw.Listen(ctx)
	require.NoError(t, err)

	proc := &lifecycle.Process{Hooks: lifecycle.NewHooks(t.TempDir()), Out: io.Discard}
	limits := make(chan bool, 1)
	done := make(chan error, 1)

	go func() {
		done <- serve(ctx, gw, listeners, proc, 0, nil, restOnly(func(saveCtx, closeCtx context.Context) {
			_, saveLimit := saveCtx.Deadline()
			_, closeLimit := closeCtx.Deadline()
			limits <- saveLimit || closeLimit
		}))
	}()

	stop()

	require.NoError(t, <-done)
	assert.False(t, <-limits)
}

// hangingGateway is a gateway whose one service holds every call until release
// closes or the connection ends. started closes when the first call arrives.
func hangingGateway(t *testing.T, release <-chan struct{}) (gw *gateway.Gateway, started chan struct{}) {
	t.Helper()

	started = make(chan struct{})

	var once sync.Once

	reg := service.Registration{
		Metadata: service.Metadata{
			Name: testService, SigningName: testService, EndpointPrefix: testService,
			Protocols: []protocol.Kind{protocol.KindQuery},
		},
		Serve: func(w http.ResponseWriter, rc *request.Context) {
			once.Do(func() { close(started) })

			select {
			case <-release:
				_, _ = io.WriteString(w, "done")
			case <-rc.Request.Context().Done():
			}
		},
	}

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{anyLoopbackPort}} },
		[]service.Registration{reg}, nil, nil,
	)
	require.NoError(t, err)

	return gw, started
}

// callHangingService sends one call to addr with c and reports the body it got,
// or the empty string when the call was cut.
func callHangingService(t *testing.T, c *http.Client, addr string) <-chan string {
	t.Helper()

	got := make(chan string, 1)

	go func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+"/",
			strings.NewReader("Action=Hang"))
		if err != nil {
			got <- ""

			return
		}

		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=t/20240101/us-east-1/"+testService+
			"/aws4_request, SignedHeaders=host, Signature=abc")

		resp, err := c.Do(req)
		if err != nil {
			got <- ""

			return
		}

		defer func() { _ = resp.Body.Close() }()

		body, _ := io.ReadAll(resp.Body)
		got <- string(body)
	}()

	return got
}

// Test_serve_listenerFailureKeepsToOneBudget: a listener that fails starts the
// shutdown inside the gateway. The drain, the hooks, the save and Close then
// share the one budget counted from the failure, and the save has its deadline
// KillMargin before the end of it. Before the plan was shared, the gateway
// drained on a timeout of its own, and a second full budget followed.
//
// It runs on synctest's clock, over an in-memory listener, so the plan's
// instants are exact and load cannot move them.
func Test_serve_listenerFailureKeepsToOneBudget(t *testing.T) {
	synctest.Test(t, serveListenerFailure)
}

func serveListenerFailure(t *testing.T) {
	const (
		timeout = 2 * time.Second
		// The plan for 2 seconds: work ends at 1s, the in-process calls at 1.25s,
		// the save at 1.5s, Close at 1.9s.
		workAt  = time.Second
		saveBy  = timeout - lifecycle.KillMargin
		closeBy = timeout - lifecycle.CloseMargin
	)

	gw, started := hangingGateway(t, make(chan struct{}))

	ctx, stop := context.WithCancel(t.Context())
	defer stop()

	mem := newMemListener()
	listeners := []net.Listener{mem}

	type saved struct {
		at           time.Time
		saveBy       time.Time
		closeBy      time.Time
		hasSaveLimit bool
	}

	saves := make(chan saved, 1)
	proc := &lifecycle.Process{Hooks: lifecycle.NewHooks(t.TempDir()), Out: io.Discard}
	done := make(chan error, 1)

	go func() {
		done <- serve(ctx, gw, listeners, proc, timeout, nil, restOnly(func(saveCtx, closeCtx context.Context) {
			saveDeadline, hasLimit := saveCtx.Deadline()
			closeDeadline, _ := closeCtx.Deadline()

			saves <- saved{at: time.Now(), saveBy: saveDeadline, closeBy: closeDeadline, hasSaveLimit: hasLimit}
		}))
	}()

	call := callHangingService(t, mem.client(), "mem")

	<-started

	// The listener fails; ctx is still live, so nothing but the failure starts the
	// shutdown.
	failedAt := time.Now()

	require.NoError(t, mem.Close())

	got := <-saves

	assert.GreaterOrEqual(t, got.at.Sub(failedAt), workAt, "the drain waited for the planned Work deadline")
	assert.Less(t, got.at.Sub(failedAt), saveBy, "the save started inside one budget, not after a second one")
	require.True(t, got.hasSaveLimit)
	assert.Equal(t, saveBy, got.saveBy.Sub(failedAt), "the save ends KillMargin before the kill")
	assert.Equal(t, closeBy, got.closeBy.Sub(failedAt), "Close ends CloseMargin before the kill")
	assert.Empty(t, <-call, "the request in flight was cut at Work")

	require.Error(t, <-done, "the failed listener is serve's error")
	assert.Less(t, time.Since(failedAt), timeout, "serve returned before the supervisor's SIGKILL")
}

// Test_serve_withoutATimeoutWaitsForEverything: SHUTDOWN_TIMEOUT=0 is no limit
// for the hooks, the drain, the save and Close alike. The gateway has no cap of
// its own to cut a request that takes a while.
func Test_serve_withoutATimeoutWaitsForEverything(t *testing.T) {
	const held = 500 * time.Millisecond

	release := make(chan struct{})
	gw, started := hangingGateway(t, release)

	root := t.TempDir()
	hook := filepath.Join(root, "shutdown.d", "slow.sh")

	require.NoError(t, os.MkdirAll(filepath.Dir(hook), 0o750))
	require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\nexec sleep 0.3\n"), 0o700))

	ctx, stop := context.WithCancel(t.Context())
	defer stop()

	listeners, err := gw.Listen(ctx)
	require.NoError(t, err)

	type saved struct {
		at     time.Time
		limits bool
	}

	saves := make(chan saved, 1)
	proc := &lifecycle.Process{Hooks: lifecycle.NewHooks(root), Out: io.Discard}
	done := make(chan error, 1)

	go func() {
		done <- serve(ctx, gw, listeners, proc, 0, nil, restOnly(func(saveCtx, closeCtx context.Context) {
			_, saveLimit := saveCtx.Deadline()
			_, closeLimit := closeCtx.Deadline()

			saves <- saved{at: time.Now(), limits: saveLimit || closeLimit}
		}))
	}()

	call := callHangingService(t, http.DefaultClient, listeners[0].Addr().String())

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never reached the service")
	}

	begin := time.Now()

	stop()
	time.AfterFunc(held, func() { close(release) })

	var got saved

	select {
	case got = <-saves:
	case <-time.After(10 * time.Second):
		t.Fatal("the state was never saved")
	}

	assert.GreaterOrEqual(t, got.at.Sub(begin), held, "the save waited for the request in flight")
	assert.False(t, got.limits, "neither the save nor Close has a deadline")
	assert.Equal(t, "done", <-call, "the request finished instead of being cut")
	require.NoError(t, <-done)
}

// stamped is a persisted service whose snapshots are counted.
type stamped struct {
	snapshots atomic.Int32
}

func (s *stamped) Snapshot(out io.Writer) error {
	s.snapshots.Add(1)

	_, err := io.WriteString(out, "state")

	return err //nolint:wrapcheck // a test double
}

func (*stamped) Restore(io.Reader) error { return nil }

func Test_saver_save(t *testing.T) {
	newSaver := func(t *testing.T, closeFn func(context.Context)) (*saver, *stamped) {
		t.Helper()

		eng, _ := persistentEngine(t)
		snap := &stamped{}
		reg := registration(testService)
		reg.Persist = snap
		reg.Close = closeFn

		eng.Register(testService, snap)
		eng.MarkDirty(testService)

		return &saver{eng: eng, services: &providers{regs: []service.Registration{reg}}}, snap
	}

	// It runs on synctest's clock, so the Close deadline is met to the instant.
	t.Run("a Close that never returns does not delay the save and is abandoned at its deadline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			stuck := make(chan struct{})
			savedBeforeClose := make(chan int32, 1)

			t.Cleanup(func() { close(stuck) })

			var (
				s    *saver
				snap *stamped
			)

			// It ignores its context, as a Close that waits for goroutines does.
			s, snap = newSaver(t, func(context.Context) {
				savedBeforeClose <- snap.snapshots.Load()

				<-stuck
			})

			begin := time.Now()

			saveCtx, cancelSave := context.WithDeadline(t.Context(), begin.Add(time.Second))
			defer cancelSave()

			closeCtx, cancelClose := context.WithDeadline(t.Context(), begin.Add(400*time.Millisecond))
			defer cancelClose()

			s.save(saveCtx, closeCtx)

			assert.Equal(t, 400*time.Millisecond+closeGrace, time.Since(begin),
				"save waited for Close until its deadline and the grace after it, and no longer")
			assert.EqualValues(t, 1, snap.snapshots.Load(), "the state was saved")

			select {
			case saved := <-savedBeforeClose:
				assert.EqualValues(t, 1, saved, "the save was done before Close began")
			default:
				t.Fatal("Close never ran")
			}
		})
	})

	t.Run("a Close that finishes in time is followed by a last save of what it changed", func(t *testing.T) {
		var s *saver

		s, snap := newSaver(t, func(context.Context) {
			s.eng.MarkDirty(testService) //nolint:contextcheck // the flush timer has no caller context
		})

		s.save(t.Context(), t.Context())

		assert.EqualValues(t, 2, snap.snapshots.Load(), "the save, and the last save after Close")
	})

	t.Run("without a deadline Close takes the time it needs", func(t *testing.T) {
		var finished atomic.Bool

		s, snap := newSaver(t, func(context.Context) {
			time.Sleep(300 * time.Millisecond)
			finished.Store(true)
		})

		s.save(t.Context(), context.WithoutCancel(t.Context()))

		assert.True(t, finished.Load(), "save waited for Close to return")
		assert.EqualValues(t, 1, snap.snapshots.Load())
	})

	t.Run("it runs once", func(t *testing.T) {
		s, snap := newSaver(t, nil)

		s.save(t.Context(), t.Context())
		s.eng.MarkDirty(testService)
		s.save(t.Context(), t.Context())

		assert.EqualValues(t, 1, snap.snapshots.Load())
	})
}

// Test_saver_save_closesEveryServiceAtOnce runs a Close that holds on until its
// context ends beside a fast one, listed after it: neither waits for the other,
// both get the whole Close budget, and the shutdown ends at that budget with the
// fast service's state saved.
func Test_saver_save_closesEveryServiceAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			fastService = "fast"
			slowService = "slow"
		)

		eng, dir := persistentEngine(t)
		state := &stamped{}

		eng.Register(fastService, state)
		eng.MarkDirty(fastService)

		// Each Close sends the deadline it was given and the time it returned.
		slowSaw, fastSaw := make(chan [2]time.Time, 1), make(chan [2]time.Time, 1)

		slow := registration(slowService)
		slow.Close = func(ctx context.Context) {
			deadline, _ := ctx.Deadline()

			<-ctx.Done()

			slowSaw <- [2]time.Time{deadline, time.Now()}
		}

		fast := registration(fastService)
		fast.Persist = state
		fast.Close = func(ctx context.Context) {
			deadline, _ := ctx.Deadline()

			fastSaw <- [2]time.Time{deadline, time.Now()}
		}

		s := &saver{eng: eng, services: &providers{regs: []service.Registration{slow, fast}}}

		begin := time.Now()
		closeBy := begin.Add(400 * time.Millisecond)

		saveCtx, cancelSave := context.WithDeadline(t.Context(), begin.Add(200*time.Millisecond))
		defer cancelSave()

		closeCtx, cancelClose := context.WithDeadline(t.Context(), closeBy)
		defer cancelClose()

		s.save(saveCtx, closeCtx)

		assert.Equal(t, closeBy, time.Now(), "the shutdown ends at the Close budget")

		slowGot, fastGot := <-slowSaw, <-fastSaw
		assert.Equal(t, closeBy, slowGot[0], "the slow Close has the whole budget")
		assert.Equal(t, closeBy, slowGot[1], "and returns at its end")
		assert.Equal(t, closeBy, fastGot[0], "the fast Close has the whole budget too")
		assert.Equal(t, begin, fastGot[1], "and did not wait for the slow one")

		assert.EqualValues(t, 1, state.snapshots.Load(), "the fast service's state was saved once")
		assert.FileExists(t, filepath.Join(dir, fastService, "store.json"))
	})
}
