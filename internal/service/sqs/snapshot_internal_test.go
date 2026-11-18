package sqs

import (
	"bytes"
	"testing"
	"testing/synctest"
	"time"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_Restore_arms_the_timer_of_each_queue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := New(nil)
		ctx := callContext(t)

		created, err := source.CreateQueue(ctx, &sqsapi.CreateQueueInput{QueueName: new("q")})
		require.NoError(t, err)

		_, err = source.SendMessage(ctx, &sqsapi.SendMessageInput{
			QueueUrl: created.QueueUrl, MessageBody: new("delayed"), DelaySeconds: new(int32(5)),
		})
		require.NoError(t, err)

		_, err = source.SendMessage(ctx, &sqsapi.SendMessageInput{QueueUrl: created.QueueUrl, MessageBody: new("hidden")})
		require.NoError(t, err)

		out, err := source.ReceiveMessage(ctx, &sqsapi.ReceiveMessageInput{
			QueueUrl: created.QueueUrl, VisibilityTimeout: new(int32(30)),
		})
		require.NoError(t, err)
		require.Len(t, out.Messages, 1)

		var snapshot bytes.Buffer

		require.NoError(t, source.Snapshot(&snapshot))

		restored := New(nil)
		require.NoError(t, restored.Restore(&snapshot))

		q, found := restored.states.Get("000000000000", "us-east-1").find("q")
		require.True(t, found)

		armed, pending := timerOf(t, q)
		assert.True(t, armed, "the restored deadlines have a timer")
		assert.Equal(t, 2, pending, "the delayed and the in-flight message")

		// Nothing calls the queue: only the timer can move the delayed message.
		time.Sleep(5 * time.Second)
		synctest.Wait()

		armed, pending = timerOf(t, q)
		assert.True(t, armed)
		assert.Equal(t, 1, pending, "the delayed message became visible by itself")

		time.Sleep(25 * time.Second)
		synctest.Wait()

		armed, pending = timerOf(t, q)
		assert.False(t, armed, "no deadline is left, so no timer")
		assert.Zero(t, pending)
	})
}

func Test_every_kind_of_queue_can_be_saved(t *testing.T) {
	p := New(nil)
	ctx := callContext(t)

	for name, attrs := range map[string]map[string]string{
		"standard":   nil,
		"first.fifo": {attrFifoQueue: "true"},
	} {
		_, err := p.CreateQueue(ctx, &sqsapi.CreateQueueInput{QueueName: new(name), Attributes: attrs})
		require.NoError(t, err)

		q, found := p.states.Get("000000000000", "us-east-1").find(name)
		require.True(t, found)

		_, ok := q.contents.(persistentContents)
		assert.True(t, ok, "the contents of %s must implement persistentContents, or its messages are lost on restart", name)
	}
}

func Test_Snapshot_and_Restore_refuse_contents_they_cannot_save(t *testing.T) {
	p := New(nil)
	p.contents = func(*Queue) Contents { return &fakeContents{} }

	_, err := p.CreateQueue(callContext(t), &sqsapi.CreateQueueInput{QueueName: new("q")})
	require.NoError(t, err)

	var buf bytes.Buffer

	require.ErrorContains(t, p.Snapshot(&buf), "its contents cannot be saved")
	assert.Zero(t, buf.Len(), "nothing is written for a snapshot that failed")

	good := New(nil)
	_, err = good.CreateQueue(callContext(t), &sqsapi.CreateQueueInput{QueueName: new("q")})
	require.NoError(t, err)
	require.NoError(t, good.Snapshot(&buf))

	require.ErrorContains(t, p.Restore(&buf), "its contents cannot be restored")
}
