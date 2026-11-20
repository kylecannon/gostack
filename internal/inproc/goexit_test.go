package inproc_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/inproc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bound is how long these tests wait for something that, when it works,
// happens at once. They wait on real time rather than in a synctest bubble: a
// worker that dies without answering leaves goroutines blocked for good, which
// a bubble reports as a deadlock instead of the assertion that failed.
const bound = 5 * time.Second

// exitPath is the path of a call whose handler calls runtime.Goexit, as a
// test that calls require.FailNow in a handler does.
const exitPath = "/exit"

// goexiting calls runtime.Goexit for a call to exitPath and answers any other
// with a 200.
func goexiting(_ http.ResponseWriter, r *http.Request) {
	if r.URL.Path == exitPath {
		runtime.Goexit()
	}
}

// answer is what one queued call's done was given.
type answer struct {
	path   string
	status int
	err    error
}

// Test_Enqueue_AHandlerThatCallsGoexitFailsItsCallAndTheLaneGoesOn queues, in
// twice as many lanes as there are workers, a call whose handler calls
// runtime.Goexit and then one that succeeds. Each Goexit call is answered with
// ErrAborted, the call after it in its lane is served, the queue goes idle and
// the hub's count falls to zero, so a drain returns at once with nothing cut.
func Test_Enqueue_AHandlerThatCallsGoexitFailsItsCallAndTheLaneGoesOn(t *testing.T) {
	const lanes = 2 * inproc.MaxInFlight

	hub := inproc.NewHub(http.HandlerFunc(goexiting))
	client := inproc.New()
	t.Cleanup(client.Close)
	client.Bind(hub)

	answers := make(chan answer, 2*lanes)

	for i := range lanes {
		for _, path := range []string{exitPath, "/ok"} {
			require.True(t, client.Enqueue(inproc.Call{Path: path, Lane: fmt.Sprint("lane", i)},
				func(resp inproc.Response, err error) { answers <- answer{path: path, status: resp.Status, err: err} }))
		}
	}

	timeout := time.After(bound)

	for range 2 * lanes {
		select {
		case got := <-answers:
			if got.path == exitPath {
				require.ErrorIs(t, got.err, inproc.ErrAborted, "a Goexit fails its call")
			} else {
				require.NoError(t, got.err, "the call after a Goexit in its lane is served")
				assert.Equal(t, http.StatusOK, got.status)
			}
		case <-timeout:
			require.FailNow(t, "a call was never answered", "%d lanes, a Goexit then a success in each", lanes)
		}
	}

	require.Eventually(t, client.Idle, bound, time.Millisecond, "the workers exit once the lanes are empty")

	ctx, cancel := context.WithTimeout(t.Context(), bound)
	defer cancel()

	assert.Zero(t, hub.Drain(ctx), "no call is left for the drain to cut")
	assert.NoError(t, ctx.Err(), "the drain found the hub idle and did not wait for its deadline")
}

// records is a slog.Handler that keeps the messages it is given.
type records struct {
	mu       sync.Mutex
	messages []string
}

func (*records) Enabled(context.Context, slog.Level) bool { return true }

func (r *records) Handle(_ context.Context, record slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.messages = append(r.messages, record.Level.String()+" "+record.Message)

	return nil
}

func (r *records) WithAttrs([]slog.Attr) slog.Handler { return r }

func (r *records) WithGroup(string) slog.Handler { return r }

func (r *records) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.messages...)
}

// childEnv names the test that Test_Enqueue_ADoneThatPanicsIsLoggedAndTheLaneGoesOn
// runs in a process of its own.
const childEnv = "INPROC_DONE_PANIC_CHILD"

// Test_Enqueue_ADoneThatPanicsIsLoggedAndTheLaneGoesOn runs
// Test_Enqueue_ADoneThatPanics in a process of its own, since a panic that
// escapes a worker ends the whole process, and with it every other test.
func Test_Enqueue_ADoneThatPanicsIsLoggedAndTheLaneGoesOn(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*bound)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^Test_Enqueue_ADoneThatPanics$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), childEnv+"=1")

	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "the process survives a done that panics:\n%s", out)
	assert.Contains(t, string(out), "--- PASS: Test_Enqueue_ADoneThatPanics", "the child ran the test:\n%s", out)
}

// Test_Enqueue_ADoneThatPanics: a done that panics is logged once, at error,
// and the next call in its lane is still served and answered.
func Test_Enqueue_ADoneThatPanics(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip("run by Test_Enqueue_ADoneThatPanicsIsLoggedAndTheLaneGoesOn in a process of its own")
	}

	log := &records{}
	previous := slog.Default()

	slog.SetDefault(slog.New(log))
	t.Cleanup(func() { slog.SetDefault(previous) })

	client := inproc.New()
	t.Cleanup(client.Close)
	client.Bind(http.HandlerFunc(goexiting))

	answered := make(chan error, 1)

	require.True(t, client.Enqueue(inproc.Call{Path: "/first", Lane: "lane"}, func(inproc.Response, error) {
		panic("done blew up")
	}))
	require.True(t, client.Enqueue(inproc.Call{Path: "/second", Lane: "lane"}, func(_ inproc.Response, err error) {
		answered <- err
	}))

	select {
	case err := <-answered:
		require.NoError(t, err, "the call after the panicking done is served")
	case <-time.After(bound):
		require.FailNow(t, "the call after the panicking done was never answered")
	}

	require.Eventually(t, client.Idle, bound, time.Millisecond, "the workers exit once the lane is empty")
	assert.Equal(t, []string{"ERROR panic in the done of a queued call"}, log.list())
}

// Test_Stream_AHandlerThatCallsGoexitIsAnError checks that Stream treats a
// handler that calls runtime.Goexit as one that panics: before the head is
// fixed it is the error of Stream, and after it the body ends with ErrAborted.
// Neither waits for the caller's context to end.
func Test_Stream_AHandlerThatCallsGoexitIsAnError(t *testing.T) {
	client := inproc.New()
	t.Cleanup(client.Close)

	client.Bind(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/late" {
			_, _ = io.WriteString(w, "partial")
		}

		runtime.Goexit()
	}))

	type streamed struct {
		got []byte
		err error
	}

	stream := func(path string) <-chan streamed {
		out := make(chan streamed, 1)

		go func() {
			resp, err := client.Stream(t.Context(), inproc.Call{Path: path}, nil, 0)
			if err != nil {
				out <- streamed{err: err}

				return
			}

			got, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			out <- streamed{got: got, err: err}
		}()

		return out
	}

	tests := []struct {
		name     string
		givePath string
		wantBody string
	}{
		{name: "before the head", givePath: "/early"},
		{name: "after the head", givePath: "/late", wantBody: "partial"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			select {
			case got := <-stream(tt.givePath):
				require.ErrorIs(t, got.err, inproc.ErrAborted)
				assert.Equal(t, tt.wantBody, string(got.got))
			case <-time.After(bound):
				require.FailNow(t, "Stream never returned")
			}
		})
	}
}
