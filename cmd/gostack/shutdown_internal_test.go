package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
func Test_serve_savesBeforeTheKill(t *testing.T) {
	const (
		timeout = 3 * time.Second
		// The plan for 3 seconds: work ends at 1.875s, the save at 2.5s.
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
				done <- serve(ctx, gw, listeners, proc, timeout, func(saveCtx context.Context) {
					deadline, hasLimit := saveCtx.Deadline()
					saves <- saved{at: time.Now(), deadline: deadline, hasLimit: hasLimit}
				})
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

			sinceSignal := got.at.Sub(begin)

			assert.GreaterOrEqual(t, sinceSignal, drainFloor, "the drain used the time before the save reserve")
			assert.Less(t, sinceSignal, saveBy, "the save started before the supervisor's SIGKILL, with its margin")
			require.True(t, got.hasLimit, "the save has a deadline")
			assert.LessOrEqual(t, got.deadline.Sub(begin), saveBy+50*time.Millisecond,
				"the save ends KillMargin before the kill")

			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("serve did not return")
			}

			assert.Less(t, time.Since(begin), saveBy, "serve returned before the supervisor's SIGKILL")
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
		done <- serve(ctx, gw, listeners, proc, 0, func(saveCtx context.Context) {
			_, hasLimit := saveCtx.Deadline()
			limits <- hasLimit
		})
	}()

	stop()

	require.NoError(t, <-done)
	assert.False(t, <-limits)
}
