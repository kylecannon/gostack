package sqs

import (
	"testing"
	"testing/synctest"
	"time"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// timerOf returns whether the queue's contents hold an armed timer, and how many
// messages have a deadline.
func timerOf(t *testing.T, q *Queue) (armed bool, pending int) {
	t.Helper()

	q.mu.Lock()
	defer q.mu.Unlock()

	contents, ok := q.contents.(*standardContents)
	require.True(t, ok, "a standard queue has standard contents")

	return contents.timerArmed(), contents.deadlines.len()
}

func Test_a_queue_with_no_deadline_has_no_timer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := New(nil, nil)
		ctx := callContext(t)

		created, err := p.CreateQueue(ctx, &sqsapi.CreateQueueInput{QueueName: new("q")})
		require.NoError(t, err)

		q, found := p.states.Get("000000000000", "us-east-1").find("q")
		require.True(t, found)

		assertIdle := func(step string) {
			t.Helper()

			armed, pending := timerOf(t, q)
			assert.False(t, armed, step)
			assert.Zero(t, pending, step)
		}

		receive := func(visibility int32) *sqsapi.ReceiveMessageOutput {
			out, recvErr := p.ReceiveMessage(ctx, &sqsapi.ReceiveMessageInput{
				QueueUrl: created.QueueUrl, VisibilityTimeout: &visibility,
			})
			require.NoError(t, recvErr)

			return out
		}

		assertIdle("a new queue")

		_, err = p.SendMessage(ctx, &sqsapi.SendMessageInput{QueueUrl: created.QueueUrl, MessageBody: new("a")})
		require.NoError(t, err)
		assertIdle("a visible message waits for a receive, not for a clock")

		out := receive(30)
		require.Len(t, out.Messages, 1)

		armed, pending := timerOf(t, q)
		assert.True(t, armed, "an in-flight message has a deadline")
		assert.Equal(t, 1, pending)

		_, err = p.DeleteMessage(ctx, &sqsapi.DeleteMessageInput{
			QueueUrl: created.QueueUrl, ReceiptHandle: out.Messages[0].ReceiptHandle,
		})
		require.NoError(t, err)
		assertIdle("after the delete")

		_, err = p.SendMessage(ctx, &sqsapi.SendMessageInput{
			QueueUrl: created.QueueUrl, MessageBody: new("b"), DelaySeconds: new(int32(5)),
		})
		require.NoError(t, err)

		armed, _ = timerOf(t, q)
		assert.True(t, armed, "a delayed message has a deadline")

		time.Sleep(5 * time.Second)
		synctest.Wait()
		assertIdle("once the delay is over")

		out = receive(2)
		require.Len(t, out.Messages, 1)
		time.Sleep(2 * time.Second)
		synctest.Wait()
		assertIdle("once the visibility timeout is over")

		out = receive(60)
		require.Len(t, out.Messages, 1)

		_, err = p.PurgeQueue(ctx, &sqsapi.PurgeQueueInput{QueueUrl: created.QueueUrl})
		require.NoError(t, err)
		assertIdle("after a purge")

		// Nothing is left to wake up, whatever the clock does.
		time.Sleep(time.Hour)
		assertIdle("an hour later")
	})
}

func Test_the_timer_follows_the_earliest_deadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := New(nil, nil)
		ctx := callContext(t)

		created, err := p.CreateQueue(ctx, &sqsapi.CreateQueueInput{QueueName: new("q")})
		require.NoError(t, err)

		q, _ := p.states.Get("000000000000", "us-east-1").find("q")

		for _, delay := range []int32{30, 10, 20} {
			_, err = p.SendMessage(ctx, &sqsapi.SendMessageInput{
				QueueUrl: created.QueueUrl, MessageBody: new("m"), DelaySeconds: &delay,
			})
			require.NoError(t, err)
		}

		countVisible := func() int { return q.contents.Counts().Visible }

		time.Sleep(10 * time.Second)
		synctest.Wait()
		q.mu.Lock()
		assert.Equal(t, 1, countVisible())
		q.mu.Unlock()

		time.Sleep(10 * time.Second)
		synctest.Wait()
		q.mu.Lock()
		assert.Equal(t, 2, countVisible())
		q.mu.Unlock()

		time.Sleep(10 * time.Second)
		synctest.Wait()

		armed, pending := timerOf(t, q)
		assert.False(t, armed)
		assert.Zero(t, pending)
	})
}
