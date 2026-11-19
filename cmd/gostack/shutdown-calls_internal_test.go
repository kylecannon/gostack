package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/config"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/inproc"
	"github.com/kylecannon/gostack/internal/lifecycle"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	sendMessageTarget = "AmazonSQS.SendMessage"
	s3Auth            = "AWS4-HMAC-SHA256 Credential=test/20250101/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host, Signature=0"
)

// logBuffer is slog's default output for one test.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p) //nolint:wrapcheck // bytes.Buffer.Write never fails
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// captureLog sends slog's default logger to a buffer until the test ends.
func captureLog(t *testing.T) *logBuffer {
	t.Helper()

	buf := &logBuffer{}
	previous := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	return buf
}

var listening = regexp.MustCompile(`msg="gostack listening" addresses=\[([^\]\s]+)\]`)

// running is a gostack that start serves in this process.
type running struct {
	base string
	stop func()
}

// startInProcess runs start with the environment the test has set, and
// returns once it listens. The nth start of the test is the nth address
// logged.
func startInProcess(t *testing.T, log *logBuffer, nth int) *running {
	t.Helper()

	cfg, err := config.Load()
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() { done <- start(ctx, cfg, io.Discard) }()

	var base string

	require.Eventually(t, func() bool {
		found := listening.FindAllStringSubmatch(log.String(), -1)
		if len(found) < nth {
			return false
		}

		base = "http://" + found[nth-1][1]

		return true
	}, 10*time.Second, 5*time.Millisecond, "gostack never listened:\n%s", log.String())

	var once sync.Once

	stop := func() {
		once.Do(func() {
			cancel()
			require.NoError(t, <-done)
		})
	}

	t.Cleanup(stop)

	return &running{base: base, stop: stop}
}

func (g *running) send(t *testing.T, method, path, body string, header map[string]string) (int, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, g.base+path, strings.NewReader(body))
	require.NoError(t, err)

	for name, value := range header {
		req.Header.Set(name, value)
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return resp.StatusCode, string(data)
}

func (g *running) s3OK(t *testing.T, method, path, body string) string {
	t.Helper()

	status, data := g.send(t, method, path, body, map[string]string{"Authorization": s3Auth})
	require.Less(t, status, http.StatusMultipleChoices, "%s %s: %s", method, path, data)

	return data
}

func (g *running) sqsOK(t *testing.T, operation string, params map[string]any) map[string]any {
	t.Helper()

	encoded, err := json.Marshal(params)
	require.NoError(t, err)

	status, data := g.send(t, http.MethodPost, "/", string(encoded), map[string]string{
		"Content-Type": "application/x-amz-json-1.0", "X-Amz-Target": "AmazonSQS." + operation,
	})
	require.Equal(t, http.StatusOK, status, "%s: %s", operation, data)

	var out map[string]any

	require.NoError(t, json.Unmarshal([]byte(data), &out))

	return out
}

// refusing reports whether a request from outside is refused: answered 503,
// or not taken at all once the listener has closed.
func (g *running) refusing(t *testing.T) bool {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, g.base+"/bucket", http.NoBody)
	require.NoError(t, err)
	req.Header.Set("Authorization", s3Auth)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return true
	}

	_ = resp.Body.Close()

	return resp.StatusCode == http.StatusServiceUnavailable
}

// Test_start_deliversANotificationHeldPastTheSignal: an S3 put raises an event
// for an SQS queue, and its delivery is held back until the shutdown has begun.
// After a clean stop with persistence on, a new process on the same directory
// has the object and the event in the queue.
func Test_start_deliversANotificationHeldPastTheSignal(t *testing.T) {
	t.Setenv("PERSIST_BASE_DIR", t.TempDir())
	t.Setenv("PERSIST_S3", "1")
	t.Setenv("PERSIST_SQS", "1")
	t.Setenv("GATEWAY_LISTEN", anyLoopbackPort)
	t.Setenv("SHUTDOWN_TIMEOUT", "10")

	log := captureLog(t)

	var armed atomic.Bool

	entered := make(chan struct{})
	release := make(chan struct{})

	previous := callHandler
	callHandler = func(gw http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Amz-Target") == sendMessageTarget && armed.CompareAndSwap(true, false) {
				close(entered)

				select {
				case <-release:
				case <-r.Context().Done():
				}
			}

			gw.ServeHTTP(w, r)
		})
	}

	t.Cleanup(func() { callHandler = previous })

	first := startInProcess(t, log, 1)

	created := first.sqsOK(t, "CreateQueue", map[string]any{"QueueName": "events"})
	require.NotEmpty(t, created["QueueUrl"])

	first.s3OK(t, http.MethodPut, "/bucket", "")
	first.s3OK(t, http.MethodPut, "/bucket?notification", `<NotificationConfiguration><QueueConfiguration>`+
		`<Id>put</Id><Queue>arn:aws:sqs:us-east-1:000000000000:events</Queue><Event>s3:ObjectCreated:*</Event>`+
		`</QueueConfiguration></NotificationConfiguration>`)

	// The configuration's s3:TestEvent went through; hold the next message.
	armed.Store(true)
	first.s3OK(t, http.MethodPut, "/bucket/held", "acknowledged")
	<-entered

	stopped := make(chan struct{})

	go func() {
		defer close(stopped)

		first.stop()
	}()

	require.Eventually(t, func() bool { return first.refusing(t) }, 10*time.Second, time.Millisecond,
		"the shutdown never began")
	close(release)
	<-stopped

	callHandler = previous
	second := startInProcess(t, log, 2)

	assert.Equal(t, "acknowledged", second.s3OK(t, http.MethodGet, "/bucket/held", ""))

	queueURL := second.sqsOK(t, "GetQueueUrl", map[string]any{"QueueName": "events"})["QueueUrl"]
	received := second.sqsOK(t, "ReceiveMessage", map[string]any{"QueueUrl": queueURL, "MaxNumberOfMessages": 10})

	messages, _ := received["Messages"].([]any)

	bodies := make([]string, 0, len(messages))

	for _, m := range messages {
		message, _ := m.(map[string]any)
		body, _ := message["Body"].(string)
		bodies = append(bodies, body)
	}

	require.Len(t, bodies, 2, "the test event and the held event: %v", bodies)
	assert.Contains(t, strings.Join(bodies, "\n"), `"eventName":"ObjectCreated:Put"`)
	assert.Contains(t, strings.Join(bodies, "\n"), `"key":"held"`)
}

// Test_serve_cutsQueuedCallsAtTheirDeadline: a thousand calls queued for a
// destination that never answers cannot hold the shutdown. serve returns
// inside SHUTDOWN_TIMEOUT, the save runs with its reserve, and the log says how
// many calls were cut.
func Test_serve_cutsQueuedCallsAtTheirDeadline(t *testing.T) {
	const (
		timeout = 2 * time.Second
		// The plan for 2 seconds: work ends at 0.75s, the in-process calls at 1s,
		// the save at 1.5s.
		callsAt = time.Second
		saveBy  = timeout - lifecycle.KillMargin
		slack   = 100 * time.Millisecond
	)

	log := captureLog(t)

	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })

	var reached atomic.Int32

	reg := service.Registration{
		Metadata: service.Metadata{
			Name: testService, SigningName: testService, EndpointPrefix: testService,
			Protocols: []protocol.Kind{protocol.KindQuery},
		},
		// A slow destination that ignores its request's context.
		Serve: func(http.ResponseWriter, *request.Context) {
			reached.Add(1)
			<-stuck
		},
	}

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{anyLoopbackPort}} },
		[]service.Registration{reg}, nil, nil,
	)
	require.NoError(t, err)

	hub := inproc.NewHub(gw.InternalHandler())
	client := inproc.New()
	client.Bind(hub)
	gw.DrainCalls(func(ctx context.Context) { hub.Drain(ctx) })

	for i := range 1000 {
		require.True(t, client.Enqueue(inproc.Call{
			Account: "000000000000", Region: "us-east-1", Service: testService,
			Path: "/" + string(rune('a'+i%26)), Body: []byte("Action=Slow"),
			Header: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}},
		}, nil))
	}

	ctx, stop := context.WithCancel(t.Context())
	defer stop()

	listeners, err := gw.Listen(ctx)
	require.NoError(t, err)

	type saved struct {
		at     time.Time
		saveBy time.Time
	}

	saves := make(chan saved, 1)
	proc := &lifecycle.Process{Hooks: lifecycle.NewHooks(t.TempDir()), Out: io.Discard}
	done := make(chan error, 1)

	go func() {
		done <- serve(ctx, gw, listeners, proc, timeout, func(saveCtx, _ context.Context) {
			deadline, _ := saveCtx.Deadline()
			saves <- saved{at: time.Now(), saveBy: deadline}
		})
	}()

	require.Eventually(t, func() bool { return reached.Load() == inproc.MaxInFlight }, 5*time.Second, time.Millisecond)

	// SIGTERM.
	begin := time.Now()

	stop()

	var got saved

	select {
	case got = <-saves:
	case <-time.After(timeout + 5*time.Second):
		t.Fatal("the state was never saved")
	}

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return")
	}

	assert.Less(t, time.Since(begin), saveBy, "serve returned before the supervisor's SIGKILL, with its margin")
	assert.GreaterOrEqual(t, got.at.Sub(begin), callsAt-slack, "the queued calls had until Calls")
	assert.Less(t, got.at.Sub(begin), callsAt+slack, "the queued calls were cut at Calls, not later")
	assert.LessOrEqual(t, got.saveBy.Sub(begin), saveBy+slack, "the save keeps its reserve")
	assert.Equal(t, 1000, hub.Cut(), "every call was cut: the ones running and the ones queued")
	assert.Contains(t, log.String(), "in-process calls cut at shutdown")
	assert.Contains(t, log.String(), "count=1000")
	assert.Equal(t, int32(inproc.MaxInFlight), reached.Load(), "no queued call reached the destination")
}
