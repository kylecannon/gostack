package sqs_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	wire "github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_SendMessage_delay(t *testing.T) {
	tests := []struct {
		name       string
		giveQueue  map[string]string
		giveParams map[string]any
	}{
		{name: "on the message", giveParams: map[string]any{paramDelaySeconds: 5}},
		{name: "on the queue", giveQueue: map[string]string{attrDelaySeconds: "5"}},
		{
			name: "the message's delay replaces the queue's", giveQueue: map[string]string{attrDelaySeconds: "30"},
			giveParams: map[string]any{paramDelaySeconds: 5},
		},
	}

	for _, protocol := range protocols {
		for _, tt := range tests {
			t.Run(protocol+"/"+tt.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					c := newStack(t).as(protocol)
					url := c.createQueue("q", tt.giveQueue)
					c.send(url, "later", tt.giveParams)

					assert.Empty(t, c.receive(url, nil), "at once")

					time.Sleep(4 * time.Second)
					assert.Empty(t, c.receive(url, nil), "a second early")

					time.Sleep(time.Second)
					got := c.receive(url, nil)
					require.Len(t, got, 1, "after the delay")
					assert.Equal(t, "later", got[0].body)
				})
			})
		}
	}
}

func Test_SendMessage_delay_set_after_the_send_does_not_apply(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newStack(t).as(protocol)
				url := c.createQueue("q", nil)
				c.send(url, "now", nil)

				set := c.call("SetQueueAttributes", map[string]any{
					paramQueueURL: url, paramAttributes: map[string]string{attrDelaySeconds: "60"},
				})
				require.True(t, set.ok())

				assert.Len(t, c.receive(url, nil), 1)
			})
		})
	}
}

func Test_visibility_timeout_expires_and_the_message_comes_back(t *testing.T) {
	tests := []struct {
		name       string
		giveQueue  map[string]string
		giveParams map[string]any
		wantWait   time.Duration
	}{
		{name: "the queue's timeout", giveQueue: map[string]string{attrVisibilityTimeout: "7"}, wantWait: 7 * time.Second},
		{
			name: "the receive's timeout replaces the queue's", giveQueue: map[string]string{attrVisibilityTimeout: "30"},
			giveParams: map[string]any{paramVisibilityTimeout: 3}, wantWait: 3 * time.Second,
		},
		{name: "a timeout of zero", giveParams: map[string]any{paramVisibilityTimeout: 0}, wantWait: 0},
	}

	for _, protocol := range protocols {
		for _, tt := range tests {
			t.Run(protocol+"/"+tt.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					c := newStack(t).as(protocol)
					url := c.createQueue("q", tt.giveQueue)
					c.send(url, "a", nil)

					first := c.receive(url, tt.giveParams)
					require.Len(t, first, 1)

					if tt.wantWait > 0 {
						time.Sleep(tt.wantWait - time.Nanosecond)
						assert.Empty(t, c.receive(url, tt.giveParams), "still hidden")

						time.Sleep(time.Nanosecond)
					}

					second := c.receive(url, tt.giveParams)
					require.Len(t, second, 1, "visible again")
					assert.Equal(t, first[0].id, second[0].id)
					assert.NotEqual(t, first[0].handle, second[0].handle)
				})
			})
		}
	}
}

func Test_long_poll_wakes_on_send(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newStack(t).as(protocol)
				url := c.createQueue("q", nil)

				var (
					got     []received
					elapsed time.Duration
				)

				start := time.Now()
				done := make(chan struct{})

				go func() {
					defer close(done)

					got = c.receive(url, map[string]any{paramWaitTime: 20})
					elapsed = time.Since(start)
				}()

				synctest.Wait()
				time.Sleep(3 * time.Second)
				c.send(url, "wake up", nil)
				<-done

				require.Len(t, got, 1)
				assert.Equal(t, "wake up", got[0].body)
				assert.Equal(t, 3*time.Second, elapsed, "the poll ended with the send, not with the wait")
			})
		})
	}
}

func Test_long_poll_times_out(t *testing.T) {
	tests := []struct {
		name       string
		giveQueue  map[string]string
		giveParams map[string]any
		wantWait   time.Duration
	}{
		{name: "the call's wait", giveParams: map[string]any{paramWaitTime: 5}, wantWait: 5 * time.Second},
		{
			name: "the queue's wait", giveQueue: map[string]string{attrReceiveWait: "4"},
			wantWait: 4 * time.Second,
		},
		{
			name: "the call's wait replaces the queue's", giveQueue: map[string]string{attrReceiveWait: "10"},
			giveParams: map[string]any{paramWaitTime: 2}, wantWait: 2 * time.Second,
		},
		{
			name: "a wait of zero replaces the queue's", giveQueue: map[string]string{attrReceiveWait: "10"},
			giveParams: map[string]any{paramWaitTime: 0}, wantWait: 0,
		},
	}

	for _, protocol := range protocols {
		for _, tt := range tests {
			t.Run(protocol+"/"+tt.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					c := newStack(t).as(protocol)
					url := c.createQueue("q", tt.giveQueue)

					start := time.Now()
					got := c.receive(url, tt.giveParams)

					assert.Empty(t, got)
					assert.Equal(t, tt.wantWait, time.Since(start))
				})
			})
		}
	}
}

func Test_long_poll_wakes_when_a_visibility_timeout_ends(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newStack(t).as(protocol)
				url := c.createQueue("q", nil)
				c.send(url, "again", nil)

				first := c.receive(url, map[string]any{paramVisibilityTimeout: 3})
				require.Len(t, first, 1)

				start := time.Now()
				got := c.receive(url, map[string]any{paramWaitTime: 20})

				require.Len(t, got, 1)
				assert.Equal(t, first[0].id, got[0].id)
				assert.Equal(t, 3*time.Second, time.Since(start))
			})
		})
	}
}

func Test_long_poll_wakes_when_a_delay_ends(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newStack(t).as(protocol)
				url := c.createQueue("q", nil)
				c.send(url, "later", map[string]any{paramDelaySeconds: 6})

				start := time.Now()
				got := c.receive(url, map[string]any{paramWaitTime: 20})

				require.Len(t, got, 1)
				assert.Equal(t, 6*time.Second, time.Since(start))
			})
		})
	}
}

func Test_long_poll_returns_what_is_there_after_the_first_message(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newStack(t).as(protocol)
				url := c.createQueue("q", nil)
				c.send(url, "a", nil)
				c.send(url, "b", nil)

				start := time.Now()
				got := c.receive(url, map[string]any{paramWaitTime: 20, paramMaxMessages: 10})

				assert.Len(t, got, 2, "no wait for the other eight")
				assert.Zero(t, time.Since(start))
			})
		})
	}
}

// direct calls the provider without the gateway, with a request context that
// the test can cancel.
func direct(ctx context.Context) context.Context {
	rc := &request.Context{
		Service: "sqs", Protocol: wire.KindJSON, AccountID: defaultAccount, Region: usEast1, RequestID: "r",
		Request: httptest.NewRequestWithContext(ctx, http.MethodPost, "/", nil),
	}

	return request.With(ctx, rc)
}

func Test_long_poll_ends_when_the_request_is_cancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := sqs.New(nil, nil)
		base := t.Context()

		created, err := p.CreateQueue(direct(base), &sqsapi.CreateQueueInput{QueueName: new("q")})
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(base)
		start := time.Now()

		var (
			out     *sqsapi.ReceiveMessageOutput
			callErr error
		)

		done := make(chan struct{})

		go func() {
			defer close(done)

			out, callErr = p.ReceiveMessage(direct(ctx), &sqsapi.ReceiveMessageInput{
				QueueUrl: created.QueueUrl, WaitTimeSeconds: new(int32(20)),
			})
		}()

		synctest.Wait()
		time.Sleep(2 * time.Second)
		cancel()
		<-done

		require.NoError(t, callErr)
		assert.Empty(t, out.Messages)
		assert.Equal(t, 2*time.Second, time.Since(start))
	})
}

func Test_long_poll_ends_when_the_queue_is_deleted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := sqs.New(nil, nil)
		ctx := direct(t.Context())

		created, err := p.CreateQueue(ctx, &sqsapi.CreateQueueInput{QueueName: new("q")})
		require.NoError(t, err)

		start := time.Now()
		done := make(chan struct{})

		var (
			out     *sqsapi.ReceiveMessageOutput
			callErr error
		)

		go func() {
			defer close(done)

			out, callErr = p.ReceiveMessage(ctx, &sqsapi.ReceiveMessageInput{
				QueueUrl: created.QueueUrl, WaitTimeSeconds: new(int32(20)),
			})
		}()

		synctest.Wait()
		time.Sleep(time.Second)

		_, err = p.DeleteQueue(ctx, &sqsapi.DeleteQueueInput{QueueUrl: created.QueueUrl})
		require.NoError(t, err)
		<-done

		require.NoError(t, callErr)
		assert.Empty(t, out.Messages)
		assert.Equal(t, time.Second, time.Since(start))
	})
}

func Test_message_retention(t *testing.T) {
	tests := []struct {
		name         string
		giveEnabled  bool
		giveVisible  time.Duration
		wantReceived int
	}{
		{name: "not enabled keeps the message", giveEnabled: false, wantReceived: 1},
		{name: "enabled drops it", giveEnabled: true, wantReceived: 0},
	}

	for _, protocol := range protocols {
		for _, tt := range tests {
			t.Run(protocol+"/"+tt.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					s := newStack(t)
					s.settings.update(func(settings *sqs.Settings) { settings.EnableMessageRetentionPeriod = tt.giveEnabled })

					c := s.as(protocol)
					url := c.createQueue("q", map[string]string{attrMessageRetention: "2"})
					c.send(url, "old", nil)

					time.Sleep(3 * time.Second)

					assert.Len(t, c.receive(url, nil), tt.wantReceived)
				})
			})
		}
	}
}

func Test_message_retention_applies_to_a_message_that_was_in_flight(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newStack(t)
				s.settings.update(func(settings *sqs.Settings) { settings.EnableMessageRetentionPeriod = true })

				c := s.as(protocol)
				url := c.createQueue("q", map[string]string{attrMessageRetention: "2", attrVisibilityTimeout: "2"})
				c.send(url, "a", nil)
				c.send(url, "b", nil)

				time.Sleep(time.Second)
				require.Len(t, c.receive(url, nil), 1)

				// At 2 seconds the second message is past retention. The first is in
				// flight until 3, and goes when it is visible again.
				time.Sleep(time.Second)
				assert.Empty(t, c.receive(url, nil))

				time.Sleep(2 * time.Second)
				assert.Empty(t, c.receive(url, nil))
			})
		})
	}
}
