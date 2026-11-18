package sqs

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// panickyContents is a standard queue's contents that can panic when it
// restores, and records that it was closed.
type panickyContents struct {
	*standardContents

	panics   bool
	registry *panicRegistry
}

// panicRegistry knows every panickyContents that was made and which of them were
// closed.
type panicRegistry struct {
	mu     sync.Mutex
	made   []*panickyContents
	closed map[*panickyContents]bool
}

func (c *panickyContents) Close() {
	c.registry.mu.Lock()
	c.registry.closed[c] = true
	c.registry.mu.Unlock()

	c.standardContents.Close()
}

func (c *panickyContents) restore(data *contentsData) error {
	if c.panics {
		panic("boom")
	}

	return c.standardContents.restore(data)
}

// requireUsable runs a round trip on a provider: it makes a queue, sends a
// message and receives it.
func requireUsable(t *testing.T, p *Provider) {
	t.Helper()

	ctx := callContext(t)

	created, err := p.CreateQueue(ctx, &sqsapi.CreateQueueInput{QueueName: new("after")})
	require.NoError(t, err)

	_, err = p.SendMessage(ctx, &sqsapi.SendMessageInput{QueueUrl: created.QueueUrl, MessageBody: new("hello")})
	require.NoError(t, err)

	got, err := p.ReceiveMessage(ctx, &sqsapi.ReceiveMessageInput{QueueUrl: created.QueueUrl})
	require.NoError(t, err)
	require.Len(t, got.Messages, 1)
	assert.Equal(t, "hello", *got.Messages[0].Body)
}

// requireEmpty fails unless the provider holds no queue.
func requireEmpty(t *testing.T, p *Provider) {
	t.Helper()

	var buf bytes.Buffer

	require.NoError(t, p.Snapshot(&buf))
	assert.JSONEq(t, `{"v":1,"data":{"states":[]}}`, buf.String())
}

// Test_a_Restore_that_fails_leaves_SQS_empty_and_usable is SQ-52's third
// finding: whatever the snapshot, the provider is not left half full.
func Test_a_Restore_that_fails_leaves_SQS_empty_and_usable(t *testing.T) {
	good := seedSnapshot(t, kindStandard)

	tests := map[string]string{
		"a null entry":    strings.Replace(string(good), nullEntries, `"entries":[null,{`, 1),
		"a cut file":      string(good[:len(good)/2]),
		"another version": strings.Replace(string(good), `{"v":1,`, `{"v":2,`, 1),
		"data after it":   string(good) + `{}`,
		"a queue twice": strings.Replace(string(good), `"queues":[{`,
			`"queues":[{"name":"q","seq":9,"attributes":{},"contents":{"entries":[]}},{`, 1),
		"a state twice": strings.Replace(string(good), `"states":[{`,
			`"states":[{"account":"000000000000","region":"us-east-1","next":0,"queues":[]},{`, 1),
		"an invalid state": strings.Replace(string(good), `"account":"000000000000"`, `"account":""`, 1),
	}

	for name, bad := range tests {
		t.Run(name, func(t *testing.T) {
			p := New(nil, nil)
			t.Cleanup(p.Close)

			// Something to lose.
			require.NoError(t, p.Restore(bytes.NewReader(good)))

			require.Error(t, p.Restore(strings.NewReader(bad)))
			requireEmpty(t, p)
			requireUsable(t, p)
		})
	}
}

// Test_a_Restore_that_panics_closes_every_queue_it_built builds three queues and
// makes the third panic while it restores its messages.
func Test_a_Restore_that_panics_closes_every_queue_it_built(t *testing.T) {
	source := New(nil, nil)
	t.Cleanup(source.Close)

	ctx := callContext(t)

	for _, name := range []string{"a", "b", "c"} {
		created, err := source.CreateQueue(ctx, &sqsapi.CreateQueueInput{QueueName: new(name)})
		require.NoError(t, err)

		_, err = source.SendMessage(ctx, &sqsapi.SendMessageInput{
			QueueUrl: created.QueueUrl, MessageBody: new(name), DelaySeconds: new(int32(30)),
		})
		require.NoError(t, err)
	}

	var saved bytes.Buffer

	require.NoError(t, source.Snapshot(&saved))

	p := New(nil, nil)
	t.Cleanup(p.Close)

	registry := &panicRegistry{closed: make(map[*panickyContents]bool)}
	normal := p.contents

	p.contents = func(q *Queue) Contents {
		// The first two queues restore, the third panics.
		c := &panickyContents{
			standardContents: newStandardContents(q, p.settings), panics: q.name == "c", registry: registry,
		}

		registry.mu.Lock()
		registry.made = append(registry.made, c)
		registry.mu.Unlock()

		return c
	}

	var err error

	require.NotPanics(t, func() { err = p.Restore(&saved) })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "panic")

	requireEmpty(t, p)

	// The queue that panicked was closed, and the two before it were too: their
	// timers are stopped, so none fires on a queue nobody holds.
	require.Len(t, registry.made, 3)

	for _, c := range registry.made {
		assert.True(t, registry.closed[c], "queue %s is closed", c.q.name)
	}

	p.contents = normal

	requireUsable(t, p)
}
