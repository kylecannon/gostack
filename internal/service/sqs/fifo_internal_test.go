package sqs

import (
	"testing"
	"testing/synctest"
	"time"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fifoTimerOf returns whether the FIFO queue holds an armed timer, and how many
// entries wait in its heap, deduplication records included.
func fifoTimerOf(t *testing.T, q *Queue) (armed bool, pending, remembered int) {
	t.Helper()

	q.mu.Lock()
	defer q.mu.Unlock()

	contents, ok := q.contents.(*fifoContents)
	require.True(t, ok, "a FIFO queue has FIFO contents")

	return contents.timer.armed, contents.deadlines.len(), len(contents.dedup)
}

func Test_a_FIFO_queue_holds_no_timer_after_its_deduplication_window(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := New(nil)
		ctx := callContext(t)

		created, err := p.CreateQueue(ctx, &sqsapi.CreateQueueInput{
			QueueName: new("q.fifo"), Attributes: map[string]string{attrFifoQueue: "true"},
		})
		require.NoError(t, err)

		q, found := p.states.Get("000000000000", "us-east-1").find("q.fifo")
		require.True(t, found)

		armed, pending, remembered := fifoTimerOf(t, q)
		assert.False(t, armed, "a new queue")
		assert.Zero(t, pending)
		assert.Zero(t, remembered)

		for _, id := range []string{"a", "b"} {
			_, err = p.SendMessage(ctx, &sqsapi.SendMessageInput{
				QueueUrl: created.QueueUrl, MessageBody: new("m"), MessageGroupId: new("g"), MessageDeduplicationId: new(id),
			})
			require.NoError(t, err)
		}

		armed, pending, remembered = fifoTimerOf(t, q)
		assert.True(t, armed, "the window is a deadline")
		assert.Equal(t, 2, pending)
		assert.Equal(t, 2, remembered)

		time.Sleep(dedupWindow - time.Second)

		_, _, remembered = fifoTimerOf(t, q)
		assert.Equal(t, 2, remembered, "still inside the window")

		time.Sleep(2 * time.Second)

		armed, pending, remembered = fifoTimerOf(t, q)
		assert.False(t, armed, "nothing is left to wait for")
		assert.Zero(t, pending)
		assert.Zero(t, remembered)
	})
}
