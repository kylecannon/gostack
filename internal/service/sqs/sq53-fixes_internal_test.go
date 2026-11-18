package sqs

import (
	"bytes"
	"net/http"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	holdVisibility = 30
	holdPollWait   = 10
)

// Test_ChangeMessageVisibility_leaves_a_message_a_filling_poll_holds is SQ-53
// finding 3. A message that a poll holds is in none of LocalStack's collections,
// and update_visibility_timeout returns at models.py:506 for a message that is not
// in inflight: the call succeeds and changes nothing, so the message comes back
// once in the response, with the visibility the poll gives it at its end.
func Test_ChangeMessageVisibility_leaves_a_message_a_filling_poll_holds(t *testing.T) {
	for _, kind := range []queueKind{kindStandard, kindFIFO} {
		t.Run(strconv.Itoa(int(kind)), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				q := newFillQueue(t, New(nil), kind)

				require.NoError(t, q.send(callContext(t), fillFirstBody))

				// The handle of a receive that left the message visible: it names the
				// message for good.
				stale := *q.must(callContext(t), t, 0)[0].ReceiptHandle

				go func() {
					time.Sleep(fillSendDelay)

					_, err := q.p.ChangeMessageVisibility(callContext(t), &sqsapi.ChangeMessageVisibilityInput{
						QueueUrl: &q.url, ReceiptHandle: &stale, VisibilityTimeout: new(int32(0)),
					})
					assert.NoError(t, err, "LocalStack answers the call and does nothing")
				}()

				got := q.must(withHeader(t, headerOverrideWait, strconv.Itoa(holdPollWait)), t, holdVisibility)

				require.Len(t, got, 1, "the message comes once, not twice")
				assert.Equal(t, fillFirstBody, *got[0].Body)

				// The change was lost, as in LocalStack: the poll's own visibility holds.
				assert.Empty(t, q.must(callContext(t), t, 0))

				time.Sleep(holdVisibility * time.Second)

				assert.Len(t, q.must(callContext(t), t, 0), 1)
			})
		})
	}
}

// Test_ChangeMessageVisibility_works_again_after_the_poll_ends keeps the hold
// from outliving the poll.
func Test_ChangeMessageVisibility_works_again_after_the_poll_ends(t *testing.T) {
	for _, kind := range []queueKind{kindStandard, kindFIFO} {
		t.Run(strconv.Itoa(int(kind)), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				q := newFillQueue(t, New(nil), kind)

				require.NoError(t, q.send(callContext(t), fillFirstBody))

				got := q.must(withHeader(t, headerOverrideWait, strconv.Itoa(holdPollWait)), t, holdVisibility)
				require.Len(t, got, 1)

				_, err := q.p.ChangeMessageVisibility(callContext(t), &sqsapi.ChangeMessageVisibilityInput{
					QueueUrl: &q.url, ReceiptHandle: got[0].ReceiptHandle, VisibilityTimeout: new(int32(0)),
				})
				require.NoError(t, err)

				assert.Len(t, q.must(callContext(t), t, 0), 1, "the message is visible at once")
			})
		})
	}
}

// Test_a_negative_override_wait_is_the_error_LocalStack_raises is SQ-53 finding
// 4. The header goes to queue.Queue.get, which raises a ValueError for a negative
// timeout whatever the queue holds, and LocalStack answers 500 InternalError. The
// message stays on the queue, and VisibilityTimeout never applies to it.
func Test_a_negative_override_wait_is_the_error_LocalStack_raises(t *testing.T) {
	for _, kind := range []queueKind{kindStandard, kindFIFO} {
		for _, wait := range []string{"-1", "-60", "-120"} {
			t.Run(strconv.Itoa(int(kind))+" "+wait, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					q := newFillQueue(t, New(nil), kind)

					require.NoError(t, q.send(callContext(t), fillFirstBody))

					got, err := q.receive(withHeader(t, headerOverrideWait, wait), holdVisibility)

					require.Empty(t, got)

					var failure *awserr.Error

					require.ErrorAs(t, err, &failure)
					assert.Equal(t, http.StatusInternalServerError, failure.StatusCode)
					assert.Equal(t, "CommonServiceException", failure.Code)
					assert.Equal(t, "InternalError", failure.QueryCode)
					assert.Equal(t, awserr.FaultReceiver, failure.Fault)
					assert.Equal(t, "exception while calling sqs.ReceiveMessage: 'timeout' must be a non-negative number",
						failure.Message)

					// Nothing was taken.
					assert.Len(t, q.must(callContext(t), t, 0), 1)
				})
			})
		}
	}
}

// Test_a_receive_checks_the_count_before_a_negative_override_wait: LocalStack
// validates MaxNumberOfMessages before queue.receive, which is where the wait
// fails.
func Test_a_receive_checks_the_count_before_a_negative_override_wait(t *testing.T) {
	q := newFillQueue(t, New(nil), kindStandard)

	_, err := q.p.ReceiveMessage(withHeader(t, headerOverrideWait, "-60"), &sqsapi.ReceiveMessageInput{
		QueueUrl: &q.url, MaxNumberOfMessages: new(int32(maxReceiveCount + 1)),
	})

	var failure *awserr.Error

	require.ErrorAs(t, err, &failure)
	assert.Equal(t, "InvalidParameterValueException", failure.Code)
}

// invalidUTF8Tags are two keys that differ only in bytes that are not UTF-8:
// both read as U+FFFD, so a snapshot that wrote them would repeat a key.
func invalidUTF8Tags() map[string]string {
	return map[string]string{"\xff": "1", "\xfe": "2"}
}

// Test_TagQueue_refuses_tags_that_are_not_UTF_8 is SQ-53 finding 2: the tags went
// in unchecked and every save failed on the key U+FFFD that twice.
func Test_TagQueue_refuses_tags_that_are_not_UTF_8(t *testing.T) {
	tests := map[string]map[string]string{
		"two bad keys": invalidUTF8Tags(),
		"a bad key":    {"a\xff": "1"},
		"a bad value":  {"a": "1\xff"},
	}

	for name, tags := range tests {
		t.Run(name, func(t *testing.T) {
			p := New(nil)
			ctx := callContext(t)

			created, err := p.CreateQueue(ctx, &sqsapi.CreateQueueInput{QueueName: new("q")})
			require.NoError(t, err)

			_, err = p.TagQueue(ctx, &sqsapi.TagQueueInput{QueueUrl: created.QueueUrl, Tags: tags})

			requireInvalidCharacters(t, err)

			listed, err := p.ListQueueTags(ctx, &sqsapi.ListQueueTagsInput{QueueUrl: created.QueueUrl})
			require.NoError(t, err)
			assert.Empty(t, listed.Tags, "none of the tags was taken")

			var saved bytes.Buffer

			require.NoError(t, p.Snapshot(&saved), "saving never fails on a tag")
		})
	}
}

func Test_CreateQueue_refuses_tags_that_are_not_UTF_8(t *testing.T) {
	p := New(nil)
	ctx := callContext(t)

	_, err := p.CreateQueue(ctx, &sqsapi.CreateQueueInput{QueueName: new("q"), Tags: invalidUTF8Tags()})

	requireInvalidCharacters(t, err)

	_, err = p.GetQueueUrl(ctx, &sqsapi.GetQueueUrlInput{QueueName: new("q")})
	require.Error(t, err, "the queue was not made")

	var saved bytes.Buffer

	require.NoError(t, p.Snapshot(&saved))
}

func Test_tags_in_UTF_8_are_taken(t *testing.T) {
	p := New(nil)
	ctx := callContext(t)

	created, err := p.CreateQueue(ctx, &sqsapi.CreateQueueInput{
		QueueName: new("q"), Tags: map[string]string{"\u00e9": "\u00fc"},
	})
	require.NoError(t, err)

	_, err = p.TagQueue(ctx, &sqsapi.TagQueueInput{
		QueueUrl: created.QueueUrl, Tags: map[string]string{"\u65e5\u672c": "\u8a9e"},
	})
	require.NoError(t, err)

	listed, err := p.ListQueueTags(ctx, &sqsapi.ListQueueTagsInput{QueueUrl: created.QueueUrl})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"\u00e9": "\u00fc", "\u65e5\u672c": "\u8a9e"}, listed.Tags)
}

// requireInvalidCharacters checks for the error LocalStack answers a character
// SQS does not accept with.
func requireInvalidCharacters(t *testing.T, err error) {
	t.Helper()

	var failure *awserr.Error

	require.ErrorAs(t, err, &failure)
	assert.Equal(t, "InvalidMessageContents", failure.Code)
	assert.Equal(t, http.StatusBadRequest, failure.StatusCode)
	assert.Contains(t, failure.Message, "Invalid characters found. Valid unicode characters are")
}
