package sqs_test

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	longPoll      = 20
	pollStarted   = 100 * time.Millisecond
	wakeDeadline  = 5 * time.Second
	queueURLPanic = "queue-url boom"
)

// Test_Provider_Shutdown_wakes_long_polls is provider.py on_before_stop: a
// ReceiveMessage that waits for messages answers with none, at once, when the
// service shuts down, instead of waiting out its WaitTimeSeconds.
func Test_Provider_Shutdown_wakes_long_polls(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			s := newStack(t)
			c := s.as(protocol)
			url := c.createQueue("lp", nil)

			answered := make(chan result, 1)

			go func() {
				answered <- c.call("ReceiveMessage", map[string]any{paramQueueURL: url, paramWaitTime: longPoll})
			}()

			time.Sleep(pollStarted)

			start := time.Now()

			s.provider.Shutdown()

			select {
			case res := <-answered:
				assert.True(t, res.ok(), "the poll ends with 200")
				assert.Empty(t, res.messages())
				assert.Less(t, time.Since(start), wakeDeadline)
			case <-time.After(wakeDeadline):
				t.Fatal("the long poll was not woken by Shutdown")
			}
		})
	}
}

// Test_Provider_Shutdown_keeps_the_state: shutting down wakes the waiters and
// does not empty the queues, which the final flush of persistence still saves.
func Test_Provider_Shutdown_keeps_the_state(t *testing.T) {
	s := newStack(t)
	c := s.as(protoJSON)
	url := c.createQueue("kept", nil)
	c.send(url, "still here", nil)

	s.provider.Shutdown()

	messages := c.receive(url, map[string]any{paramWaitTime: longPoll})
	require.Len(t, messages, 1)
	assert.Equal(t, "still here", messages[0].body)
}

// Test_queue_URL_panic is query_api.py handle_request: a panic while serving a
// call made to a queue URL answers with an InternalError in the query XML
// envelope, not with the generic json failure the gateway gives a request it
// knows nothing about.
func Test_queue_URL_panic(t *testing.T) {
	s := newStack(t)
	c := s.as(protoJSON)
	url := c.createQueue("boom", nil)
	c.send(url, "hello", nil)

	// A receive that takes a message tells persistence, which is where this
	// panic comes from.
	var armed atomic.Bool

	s.changes.set(func() {
		if armed.Load() {
			panic(queueURLPanic)
		}
	})
	armed.Store(true)

	got := s.send(httpCall{
		target:  "/000000000000/boom",
		body:    "Action=ReceiveMessage&Version=2012-11-05",
		headers: map[string]string{devHeaderType: formType, headerAuthorization: signed("test", usEast1)},
	})

	assert.Equal(t, http.StatusInternalServerError, got.status)
	assert.Equal(t, "text/xml", got.header.Get(devHeaderType))
	assert.True(t, strings.HasPrefix(got.body, "<?xml"), got.body)
	assert.Contains(t, got.body, "<ErrorResponse")
	assert.Contains(t, got.body, "<Code>InternalError</Code>")
	assert.Contains(t, got.body, queueURLPanic)
}
