package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// notifyHeld configures bucket "bucket" to send its object-created events to
// the queue "events", both created on g.
func (g *running) notifyHeld(t *testing.T) {
	t.Helper()

	created := g.sqsOK(t, "CreateQueue", map[string]any{"QueueName": "events"})
	require.NotEmpty(t, created["QueueUrl"])

	g.s3OK(t, http.MethodPut, "/bucket", "")
	g.s3OK(t, http.MethodPut, "/bucket?notification", `<NotificationConfiguration><QueueConfiguration>`+
		`<Id>put</Id><Queue>arn:aws:sqs:us-east-1:000000000000:events</Queue><Event>s3:ObjectCreated:*</Event>`+
		`</QueueConfiguration></NotificationConfiguration>`)
}

// eventBodies receives every message of the queue "events" on g.
func (g *running) eventBodies(t *testing.T) []string {
	t.Helper()

	queueURL := g.sqsOK(t, "GetQueueUrl", map[string]any{"QueueName": "events"})["QueueUrl"]
	received := g.sqsOK(t, "ReceiveMessage", map[string]any{"QueueUrl": queueURL, "MaxNumberOfMessages": 10})

	messages, _ := received["Messages"].([]any)

	bodies := make([]string, 0, len(messages))

	for _, m := range messages {
		message, _ := m.(map[string]any)
		body, _ := message["Body"].(string)
		bodies = append(bodies, body)
	}

	return bodies
}

// dialRefused reports whether nothing listens on the address of g any more.
func (g *running) dialRefused(t *testing.T) bool {
	t.Helper()

	var dialer net.Dialer

	conn, err := dialer.DialContext(t.Context(), "tcp", strings.TrimPrefix(g.base, "http://"))
	if err != nil {
		return true
	}

	_ = conn.Close()

	return false
}

// Test_start_savesBeforeItDrainsTheCalls pins the shutdown's order through what
// a client sees. An S3 put is in flight across the signal, so it finishes while
// the requests drain, and its event notification is queued after that. The
// call is held until the first save is on disk and a little longer, and then
// it is still served, and saved by the second save: a new process on the same
// directory has the object and the event.
//
// Each of these breaks it: the calls drained before the requests (the put's
// call is refused by a hub already shut); the save after the calls drain (the
// call is held to the drain's deadline and cut); the gateway refusing internal
// calls before the calls drain, or no calls drain at all (the call is answered
// 503); and the services bound to the gateway's outside handler (503 as well).
func Test_start_savesBeforeItDrainsTheCalls(t *testing.T) {
	dir := t.TempDir()

	t.Setenv("PERSIST_BASE_DIR", dir)
	t.Setenv("PERSIST_S3", "1")
	t.Setenv("PERSIST_SQS", "1")
	t.Setenv("PERSIST_FORMAT", "json")
	t.Setenv("PERSIST_FREQUENCY", "3600")
	t.Setenv("GATEWAY_LISTEN", anyLoopbackPort)
	t.Setenv("SHUTDOWN_TIMEOUT", "10")

	log := captureLog(t)
	store := filepath.Join(dir, "s3", "store.json")

	var armed atomic.Bool

	// savedFirst says whether the object was on disk before the held call went on.
	savedFirst := make(chan bool, 1)

	previous := callHandler
	callHandler = func(gw http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Amz-Target") == sendMessageTarget && armed.CompareAndSwap(true, false) {
				saved := false

				for !saved && r.Context().Err() == nil {
					data, _ := os.ReadFile(store)
					saved = bytes.Contains(data, []byte(`"held"`))

					time.Sleep(time.Millisecond)
				}

				// Long enough for a shutdown that does not wait for the call to
				// have stopped serving it.
				time.Sleep(100 * time.Millisecond)

				savedFirst <- saved
			}

			gw.ServeHTTP(w, r)
		})
	}

	t.Cleanup(func() { callHandler = previous })

	first := startInProcess(t, log, 1)
	first.notifyHeld(t)

	// The put: its head now, and its body once the requests are draining. The
	// server answers 100 Continue when the handler first reads the body, so the
	// put is past the shutdown guard before the signal.
	var dialer net.Dialer

	conn, err := dialer.DialContext(t.Context(), "tcp", strings.TrimPrefix(first.base, "http://"))
	require.NoError(t, err)

	t.Cleanup(func() { _ = conn.Close() })

	_, err = io.WriteString(conn, "PUT /bucket/held HTTP/1.1\r\nHost: localhost\r\nAuthorization: "+s3Auth+
		"\r\nContent-Length: 12\r\nExpect: 100-continue\r\n\r\n")
	require.NoError(t, err)

	answers := bufio.NewReader(conn)

	resp, err := http.ReadResponse(answers, nil)
	require.NoError(t, err)

	_ = resp.Body.Close()
	require.Equal(t, http.StatusContinue, resp.StatusCode)

	armed.Store(true)

	stopped := make(chan struct{})

	go func() {
		defer close(stopped)

		first.stop()
	}()

	require.Eventually(t, func() bool { return first.dialRefused(t) }, 10*time.Second, time.Millisecond,
		"the requests never began to drain")

	_, err = io.WriteString(conn, "acknowledged")
	require.NoError(t, err)

	resp, err = http.ReadResponse(answers, nil)
	require.NoError(t, err)

	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "the put in flight was acknowledged")

	<-stopped

	select {
	case saved := <-savedFirst:
		assert.True(t, saved, "the first save was on disk before the calls drained")
	default:
		t.Error("the put's event was never sent")
	}

	callHandler = previous
	second := startInProcess(t, log, 2)

	assert.Equal(t, "acknowledged", second.s3OK(t, http.MethodGet, "/bucket/held", ""))

	bodies := second.eventBodies(t)

	require.Len(t, bodies, 2, "the test event and the held event: %v", bodies)
	assert.Contains(t, strings.Join(bodies, "\n"), `"key":"held"`)
}

// Test_start_cutsTheCallsAtOnceWithNoTimeout: with SHUTDOWN_TIMEOUT=0 nothing
// ends the calls drain, so it is skipped and the calls still queued are cut at
// once, as main refused them. A call that only ends when it is cancelled does
// not hold the shutdown (SQ-294, N1).
func Test_start_cutsTheCallsAtOnceWithNoTimeout(t *testing.T) {
	t.Setenv("GATEWAY_LISTEN", anyLoopbackPort)
	t.Setenv("SHUTDOWN_TIMEOUT", "0")

	log := captureLog(t)

	var armed atomic.Bool

	entered := make(chan struct{})
	giveUp := make(chan struct{})

	previous := callHandler
	callHandler = func(gw http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Amz-Target") == sendMessageTarget && armed.CompareAndSwap(true, false) {
				close(entered)

				// A destination that ends only when its call is cancelled.
				select {
				case <-r.Context().Done():
					w.WriteHeader(http.StatusServiceUnavailable)

					return
				case <-giveUp:
				}
			}

			gw.ServeHTTP(w, r)
		})
	}

	t.Cleanup(func() { callHandler = previous })

	first := startInProcess(t, log, 1)
	first.notifyHeld(t)

	armed.Store(true)
	first.s3OK(t, http.MethodPut, "/bucket/held", "acknowledged")
	<-entered

	stopped := make(chan struct{})

	go func() {
		defer close(stopped)

		first.stop()
	}()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		close(giveUp)
		<-stopped
		t.Fatal("the shutdown waited for a call that never ends")
	}

	assert.Contains(t, log.String(), "in-process calls cut at shutdown")
	assert.Contains(t, log.String(), "count=1")
}
