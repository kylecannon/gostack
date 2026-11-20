package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/lifecycle"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sqsAnswer is what a call to the in-memory SQS came back with, and when.
type sqsAnswer struct {
	status int
	body   string
	err    error
	at     time.Time
}

// sqsCall makes one SQS JSON call through c.
func sqsCall(ctx context.Context, c *http.Client, operation, body string) sqsAnswer {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://mem/", strings.NewReader(body))
	if err != nil {
		return sqsAnswer{err: err, at: time.Now()}
	}

	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "AmazonSQS."+operation)

	resp, err := c.Do(req)
	if err != nil {
		return sqsAnswer{err: err, at: time.Now()}
	}

	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)

	return sqsAnswer{status: resp.StatusCode, body: string(data), err: err, at: time.Now()}
}

// Test_serve_aListenerFailureReleasesALongPollBeforeTheSave: a ReceiveMessage
// with WaitTimeSeconds=20 is held when a listener fails. The services are told
// to let go at once, so the poll is answered (200, empty) at that instant and
// the drain that the save waits for ends with it. A poll that was not released
// would hold the drain to the end of Work, and be cut there with no answer.
//
// It runs the real SQS provider over an in-memory listener on synctest's clock,
// where the two instants cannot be moved by load.
func Test_serve_aListenerFailureReleasesALongPollBeforeTheSave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			timeout = 2 * time.Second
			limit   = 2 * timeout
		)

		provider := sqs.New(nil, nil)
		t.Cleanup(provider.Close)

		gw, err := gateway.New(
			func() gateway.Settings { return gateway.Settings{Listen: []string{anyLoopbackPort}} },
			[]service.Registration{provider.Registration()}, nil, nil,
		)
		require.NoError(t, err)

		ctx, stop := context.WithCancel(t.Context())
		defer stop()

		firsts, rests := make(chan time.Time, 1), make(chan time.Time, 1)
		proc := &lifecycle.Process{Hooks: lifecycle.NewHooks(t.TempDir()), Out: io.Discard}
		done := make(chan error, 1)
		mem := newMemListener()

		go func() {
			done <- serve(ctx, gw, []net.Listener{mem}, proc, timeout, nil, shutdownSave{
				first: func(context.Context) { firsts <- time.Now() },
				rest:  func(_, _ context.Context) { rests <- time.Now() },
			})
		}()

		synctest.Wait()

		c := mem.client()

		made := sqsCall(t.Context(), c, "CreateQueue", `{"QueueName":"held"}`)
		require.NoError(t, made.err)
		require.Equal(t, http.StatusOK, made.status, made.body)

		var created struct {
			QueueURL string `json:"QueueUrl"`
		}

		require.NoError(t, json.Unmarshal([]byte(made.body), &created))

		answered := make(chan sqsAnswer, 1)

		go func() {
			answered <- sqsCall(t.Context(), c, "ReceiveMessage",
				fmt.Sprintf(`{"QueueUrl":%q,"WaitTimeSeconds":20}`, created.QueueURL))
		}()

		synctest.Wait()

		begin := time.Now()

		_ = mem.Close()

		poll := within(t, answered, limit, "the long poll's answer")
		first := within(t, firsts, limit, "the first save")
		within(t, rests, limit, "the second save")
		require.Error(t, within(t, done, limit, "serve's return"))

		require.NoError(t, poll.err, "the poll was answered, not cut")
		assert.Equal(t, http.StatusOK, poll.status)
		assert.Equal(t, time.Duration(0), poll.at.Sub(begin), "the poll was released when the listener failed")
		// The server's shutdown polls for idle connections every millisecond, so the
		// save follows the drain by that much on the fake clock: well before Work.
		assert.Less(t, first.Sub(begin), 10*time.Millisecond, "the save started with the drain done, not at the end of Work")
	})
}
