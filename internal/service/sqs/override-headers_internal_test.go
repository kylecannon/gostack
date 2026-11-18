package sqs

import (
	"context"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	fillWait         = "10"
	fillReceiveCount = "ApproximateReceiveCount"
	fillFirstBody    = "M"
	fillSecondBody   = "N"
	fillSendDelay    = 3 * time.Second
	fillRuns         = 30
)

// fillQueue is a queue of one kind that the tests of the override headers use.
type fillQueue struct {
	p    *Provider
	url  string
	kind queueKind
}

func newFillQueue(t *testing.T, p *Provider, kind queueKind) *fillQueue {
	t.Helper()

	in := &sqsapi.CreateQueueInput{QueueName: new("fill")}
	if kind == kindFIFO {
		in = &sqsapi.CreateQueueInput{
			QueueName: new("fill.fifo"), Attributes: map[string]string{attrFifoQueue: attrValueTrue},
		}
	}

	out, err := p.CreateQueue(callContext(t), in)
	require.NoError(t, err)

	return &fillQueue{p: p, url: *out.QueueUrl, kind: kind}
}

// withHeader is a call context that carries the LocalStack header name with
// value, as LocalStack's own clients send them.
func withHeader(t *testing.T, header, value string) context.Context {
	t.Helper()

	ctx := callContext(t)

	rc, err := callOf(ctx)
	require.Nil(t, err)

	rc.Request.Header.Set(header, value)

	return ctx
}

// send sends body. A FIFO queue gets a group of its own for each body, so no
// group holds the second message back.
func (f *fillQueue) send(ctx context.Context, body string) error {
	in := &sqsapi.SendMessageInput{QueueUrl: &f.url, MessageBody: &body}
	if f.kind == kindFIFO {
		in.MessageGroupId = new("group-" + body)
		in.MessageDeduplicationId = new("dedup-" + body)
	}

	_, err := f.p.SendMessage(ctx, in)

	return err
}

// receive receives with visibility seconds, and waits as the headers of ctx ask.
func (f *fillQueue) receive(ctx context.Context, visibility int32) ([]sqsapi.Message, error) {
	out, err := f.p.ReceiveMessage(ctx, &sqsapi.ReceiveMessageInput{
		QueueUrl: &f.url, MaxNumberOfMessages: new(int32(maxReceiveCount)), VisibilityTimeout: &visibility,
		MessageSystemAttributeNames: []string{fillReceiveCount},
	})
	if err != nil {
		return nil, err
	}

	return out.Messages, nil
}

// must is receive for a call that cannot fail.
func (f *fillQueue) must(ctx context.Context, t *testing.T, visibility int32) []sqsapi.Message {
	t.Helper()

	got, err := f.receive(ctx, visibility)
	require.NoError(t, err)

	return got
}

// Test_an_override_wait_poll_never_returns_a_message_twice is SQ-40 finding 1:
// a message that is visible again, or whose timeout is short, was taken a second
// time into the same response while the poll waited for more.
func Test_an_override_wait_poll_never_returns_a_message_twice(t *testing.T) {
	tests := []struct {
		name       string
		kind       queueKind
		visibility int32
	}{
		{name: "standard queue, visibility 0", kind: kindStandard, visibility: 0},
		{name: "standard queue, visibility 1", kind: kindStandard, visibility: 1},
		{name: "FIFO queue, visibility 0", kind: kindFIFO, visibility: 0},
		{name: "FIFO queue, visibility 1", kind: kindFIFO, visibility: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				q := newFillQueue(t, New(nil, nil), tt.kind)
				send := callContext(t)

				require.NoError(t, q.send(send, fillFirstBody))

				go func() {
					time.Sleep(fillSendDelay)
					assert.NoError(t, q.send(send, fillSecondBody))
				}()

				got := q.must(withHeader(t, headerOverrideWait, fillWait), t, tt.visibility)

				bodies := make([]string, 0, len(got))
				for i := range got {
					bodies = append(bodies, *got[i].Body)
					assert.Equal(t, "1", got[i].Attributes[fillReceiveCount], "message %s is received once", *got[i].Body)
				}

				assert.Equal(t, []string{fillFirstBody, fillSecondBody}, bodies)

				// The poll gave the messages the visibility it was asked for when it
				// ended, not when it took them.
				again := q.must(callContext(t), t, 0)
				if tt.visibility == 0 {
					assert.Len(t, again, 2, "visibility 0 leaves both visible")

					return
				}

				assert.Empty(t, again, "visibility 1 hides both from the end of the poll")

				time.Sleep(time.Second)

				assert.Len(t, q.must(callContext(t), t, 0), 2, "both are visible a second after the poll ended")
			})
		})
	}
}

// Test_an_override_wait_poll_ends_when_the_queue_is_deleted covers the end of a
// filling poll whose queue went away: it returns what it took, at once.
func Test_an_override_wait_poll_ends_when_the_queue_is_deleted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFillQueue(t, New(nil, nil), kindStandard)
		send := callContext(t)

		require.NoError(t, q.send(send, fillFirstBody))

		go func() {
			time.Sleep(fillSendDelay)

			_, err := q.p.DeleteQueue(send, &sqsapi.DeleteQueueInput{QueueUrl: &q.url})
			assert.NoError(t, err)
		}()

		start := time.Now()
		got := q.must(withHeader(t, headerOverrideWait, fillWait), t, 30)

		assert.Len(t, got, 1)
		assert.Equal(t, fillSendDelay, time.Since(start))
	})
}

// Test_DeleteMessageBatch_takes_its_limit_from_the_override_count is SQ-40
// finding 2: LocalStack passes the header as the batch's limit, and the hard
// limit of ten stood in the way.
func Test_DeleteMessageBatch_takes_its_limit_from_the_override_count(t *testing.T) {
	const entries = 11

	q := newFillQueue(t, New(nil, nil), kindStandard)
	send := callContext(t)

	for i := range entries {
		require.NoError(t, q.send(send, "m"+strconv.Itoa(i)))
	}

	received := q.must(withHeader(t, headerOverrideCount, strconv.Itoa(entries)), t, 60)
	require.Len(t, received, entries)

	batch := make([]sqsapi.DeleteMessageBatchRequestEntry, 0, entries)
	for i := range received {
		batch = append(batch, sqsapi.DeleteMessageBatchRequestEntry{
			Id: new("e" + strconv.Itoa(i)), ReceiptHandle: received[i].ReceiptHandle,
		})
	}

	_, err := q.p.DeleteMessageBatch(send, &sqsapi.DeleteMessageBatchInput{QueueUrl: &q.url, Entries: batch})
	require.Error(t, err, "without the header eleven entries are too many")

	out, err := q.p.DeleteMessageBatch(
		withHeader(t, headerOverrideCount, strconv.Itoa(entries)),
		&sqsapi.DeleteMessageBatchInput{QueueUrl: &q.url, Entries: batch},
	)
	require.NoError(t, err)
	assert.Len(t, out.Successful, entries)
	assert.Empty(t, out.Failed)

	_, err = q.p.DeleteMessageBatch(
		withHeader(t, headerOverrideCount, "5"),
		&sqsapi.DeleteMessageBatchInput{QueueUrl: &q.url, Entries: batch[:6]},
	)
	require.ErrorContains(t, err, "Maximum number of entries per request are 5. You have sent 6.")
}

// Test_a_long_poll_looks_again_when_its_wait_runs_out is SQ-40 finding 3: a
// message that becomes visible at the very end of the wait was missed, because
// the wait ended without a last look at the queue. It runs many times, because
// the timer of the message and the end of the wait fire in the same instant of
// a synctest bubble, in no fixed order.
func Test_a_long_poll_looks_again_when_its_wait_runs_out(t *testing.T) {
	const wait = 20

	for range fillRuns {
		synctest.Test(t, func(t *testing.T) {
			q := newFillQueue(t, New(nil, nil), kindStandard)

			_, err := q.p.SendMessage(callContext(t), &sqsapi.SendMessageInput{
				QueueUrl: &q.url, MessageBody: new(fillFirstBody), DelaySeconds: new(int32(wait)),
			})
			require.NoError(t, err)

			out, err := q.p.ReceiveMessage(callContext(t), &sqsapi.ReceiveMessageInput{
				QueueUrl: &q.url, WaitTimeSeconds: new(int32(wait)),
			})
			require.NoError(t, err)
			assert.Len(t, out.Messages, 1, "the message that came with the end of the wait")
		})
	}
}

// Test_a_cancelled_long_poll_returns_at_once is the other half of finding 3: a
// wait that the client ended does not run on.
func Test_a_cancelled_long_poll_returns_at_once(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFillQueue(t, New(nil, nil), kindStandard)

		ctx, cancel := context.WithCancel(callContext(t))
		defer cancel()

		go func() {
			time.Sleep(fillSendDelay)
			cancel()
		}()

		start := time.Now()
		out, err := q.p.ReceiveMessage(ctx, &sqsapi.ReceiveMessageInput{QueueUrl: &q.url, WaitTimeSeconds: new(int32(20))})
		require.NoError(t, err)

		assert.Empty(t, out.Messages)
		assert.Equal(t, fillSendDelay, time.Since(start))
	})
}
