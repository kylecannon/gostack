package sqs_test

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const garbageHandle = "garbage"

func notAValidHandle(handle string) string {
	return `The input receipt handle "` + handle + `" is not a valid receipt handle.`
}

func Test_DeleteMessage(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newStack(t).as(protocol)
				url := c.createQueue("q", map[string]string{attrVisibilityTimeout: "5"})
				c.send(url, "a", nil)

				first := c.receive(url, nil)
				require.Len(t, first, 1)

				deleted := c.call(opDeleteMessage, map[string]any{paramQueueURL: url, paramReceiptHandle: first[0].handle})
				require.True(t, deleted.ok())

				// The message does not come back, and deleting again is no error.
				time.Sleep(10 * time.Second)
				assert.Empty(t, c.receive(url, nil))

				again := c.call(opDeleteMessage, map[string]any{paramQueueURL: url, paramReceiptHandle: first[0].handle})
				assert.True(t, again.ok(), "a handle that names no message is ignored")
			})
		})
	}
}

func Test_DeleteMessage_with_an_old_handle(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newStack(t).as(protocol)
				url := c.createQueue("q", map[string]string{attrVisibilityTimeout: "5"})
				c.send(url, "a", nil)

				old := c.receive(url, nil)
				require.Len(t, old, 1)

				time.Sleep(5 * time.Second)

				current := c.receive(url, nil)
				require.Len(t, current, 1)
				require.NotEqual(t, old[0].handle, current[0].handle)

				// A standard queue accepts the handle of an earlier receive.
				res := c.call(opDeleteMessage, map[string]any{paramQueueURL: url, paramReceiptHandle: old[0].handle})
				require.True(t, res.ok())

				time.Sleep(10 * time.Second)
				assert.Empty(t, c.receive(url, nil))
			})
		})
	}
}

func Test_DeleteMessage_refuses_a_bad_handle(t *testing.T) {
	foreign := base64.StdEncoding.EncodeToString([]byte("id arn:aws:sqs:us-east-1:000000000000:other mid 1.0"))
	short := base64.StdEncoding.EncodeToString([]byte("id arn"))

	for _, protocol := range protocols {
		for name, handle := range map[string]string{
			"not base64": garbageHandle, "another queue's": foreign, "too few fields": short, "blank": "",
		} {
			t.Run(protocol+"/"+name, func(t *testing.T) {
				c := newStack(t).as(protocol)
				url := c.createQueue("q", nil)

				for _, op := range []string{opDeleteMessage, opChangeMessageVisibility} {
					params := map[string]any{paramQueueURL: url, paramReceiptHandle: handle, paramVisibilityTimeout: 5}
					res := c.call(op, params)

					assert.Equal(t, http.StatusBadRequest, res.status, op)
					assert.Equal(t, codeReceiptHandleInvalid, res.errorCode(), op)
					assert.Equal(t, notAValidHandle(handle), res.errorMessage(), op)
				}
			})
		}
	}
}

func Test_ChangeMessageVisibility(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newStack(t).as(protocol)
				url := c.createQueue("q", map[string]string{attrVisibilityTimeout: "10"})
				c.send(url, "a", nil)

				got := c.receive(url, nil)
				require.Len(t, got, 1)
				handle := got[0].handle

				change := func(seconds int) result {
					return c.call(opChangeMessageVisibility, map[string]any{
						paramQueueURL: url, paramReceiptHandle: handle, paramVisibilityTimeout: seconds,
					})
				}

				// Extending it moves the deadline from now, not from the receive.
				time.Sleep(4 * time.Second)
				require.True(t, change(20).ok())

				time.Sleep(19 * time.Second)
				assert.Empty(t, c.receive(url, nil), "20 seconds have not passed")

				time.Sleep(time.Second)
				back := c.receive(url, nil)
				require.Len(t, back, 1)
				assert.Equal(t, got[0].id, back[0].id)

				// A visibility of zero makes the message visible at once.
				handle = back[0].handle
				require.True(t, change(0).ok())

				third := c.receive(url, map[string]any{paramAttributeNames: []string{attrAll}})
				require.Len(t, third, 1)
				assert.Equal(t, "3", third[0].system["ApproximateReceiveCount"])
			})
		})
	}
}

func Test_ChangeMessageVisibility_after_delete(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			url := c.createQueue("q", nil)
			c.send(url, "a", nil)

			got := c.receive(url, nil)
			require.Len(t, got, 1)

			params := map[string]any{paramQueueURL: url, paramReceiptHandle: got[0].handle, paramVisibilityTimeout: 42}
			require.True(t, c.call(opChangeMessageVisibility, params).ok())
			require.True(t, c.call(opDeleteMessage, map[string]any{paramQueueURL: url, paramReceiptHandle: got[0].handle}).ok())

			res := c.call(opChangeMessageVisibility, params)

			assert.Equal(t, http.StatusBadRequest, res.status)
			assert.Equal(t, errorCodes(protocol, codeInvalidParameterException, codeInvalidParameter), res.errorCode())
			assert.Equal(t, "Value "+got[0].handle+" for parameter ReceiptHandle is invalid. Reason: "+
				"Message does not exist or is not available for visibility timeout change.", res.errorMessage())
		})
	}
}

func Test_SendMessageBatch(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			url := c.createQueue("q", map[string]string{attrMaximumMessageSize: "2048"})

			res := c.call(opSendMessageBatch, map[string]any{paramQueueURL: url, paramEntries: []entry{
				{
					paramID: "1", paramMessageBody: "ok",
					paramMessageAttributes: attrs{"k": {DataType: typeString, StringValue: "x"}},
				},
				{paramID: "2", paramMessageBody: "bad\x01"},
				{
					paramID: "3", paramMessageBody: "ok",
					paramMessageSystemAttrs: attrs{sysTraceHeader: {DataType: typeString, StringValue: traceHeaderValue}},
				},
				{paramID: "4", paramMessageBody: strings.Repeat("a", 2049)},
				{paramID: "5", paramMessageBody: "ok", paramDelaySeconds: 1},
			}})
			require.True(t, res.ok(), "%s %s", res.errorCode(), res.errorMessage())

			successful := res.entries(listSuccessful, elementSendResult)
			assert.Equal(t, []string{"1", "3", "5"}, ids(successful))
			assert.Equal(t, md5KX, successful[0]["MD5OfMessageAttributes"])
			assert.Equal(t, bodyMD5("ok"), successful[0]["MD5OfMessageBody"])
			assert.NotEmpty(t, successful[0]["MessageId"])
			assert.Equal(t, md5Trace, successful[1]["MD5OfMessageSystemAttributes"])
			assert.Empty(t, successful[2]["MD5OfMessageAttributes"])

			failed := res.entries(listFailed, elementFailed)
			require.Len(t, failed, 2)
			assert.Equal(t, "2", failed[0]["Id"])
			assert.Equal(t, "InvalidMessageContents", failed[0]["Code"])
			assert.Equal(t, "4", failed[1]["Id"])
			assert.Equal(t, codeInvalidParameterException, failed[1]["Code"])
			assert.Equal(t, "true", failed[1]["SenderFault"])
			assert.Equal(t, "One or more parameters are invalid. Reason: Message must be shorter than 2048 bytes.",
				failed[1]["Message"])

			// The messages that went through are in the queue.
			got := c.receive(url, map[string]any{paramMaxMessages: 10})
			assert.Len(t, got, 2, "the third has a delay")
		})
	}
}

func Test_batch_requests_are_refused_as_a_whole(t *testing.T) {
	long := make([]entry, 0, 10)
	for i := range 10 {
		long = append(long, entry{paramID: strconv.Itoa(i), paramMessageBody: strings.Repeat("a", 110000)})
	}

	tests := []struct {
		name      string
		giveItems []entry
		wantJSON  string
		wantQuery string
		wantText  string
	}{
		{
			name: "empty", giveItems: nil,
			wantJSON: "EmptyBatchRequest", wantQuery: "AWS.SimpleQueueService.EmptyBatchRequest",
		},
		{
			name: "eleven entries", giveItems: numbered(11),
			wantJSON: "TooManyEntriesInBatchRequest", wantQuery: "AWS.SimpleQueueService.TooManyEntriesInBatchRequest",
			wantText: "Maximum number of entries per request are 10. You have sent 11.",
		},
		{
			name:      "an id that is not allowed",
			giveItems: []entry{{paramID: nameWithSpace, paramMessageBody: "x"}},
			wantJSON:  "InvalidBatchEntryId", wantQuery: "AWS.SimpleQueueService.InvalidBatchEntryId",
			wantText: "A batch entry id can only contain alphanumeric characters, hyphens and underscores. " +
				"It can be at most 80 letters long.",
		},
		{
			name:      "an id twice",
			giveItems: []entry{{paramID: "a", paramMessageBody: "x"}, {paramID: "a", paramMessageBody: "y"}},
			wantJSON:  "BatchEntryIdsNotDistinct", wantQuery: "AWS.SimpleQueueService.BatchEntryIdsNotDistinct",
		},
		{
			name: "more than a megabyte", giveItems: long,
			wantJSON: "BatchRequestTooLong", wantQuery: "AWS.SimpleQueueService.BatchRequestTooLong",
			wantText: "Batch requests cannot be longer than 1048576 bytes. You have sent 1100000 bytes.",
		},
	}

	for _, protocol := range protocols {
		for _, tt := range tests {
			t.Run(protocol+"/"+tt.name, func(t *testing.T) {
				c := newStack(t).as(protocol)
				url := c.createQueue("q", map[string]string{attrMaximumMessageSize: "1048576"})

				params := map[string]any{paramQueueURL: url}
				if tt.giveItems != nil {
					params[paramEntries] = tt.giveItems
				}

				res := c.call(opSendMessageBatch, params)

				assert.Equal(t, http.StatusBadRequest, res.status)
				assert.Equal(t, errorCodes(protocol, tt.wantJSON, tt.wantQuery), res.errorCode())
				assert.Equal(t, tt.wantText, res.errorMessage())
				assert.Empty(t, c.receive(url, nil), "nothing was sent")
			})
		}
	}
}

func numbered(n int) []entry {
	items := make([]entry, 0, n)
	for i := range n {
		items = append(items, entry{paramID: strconv.Itoa(i), paramMessageBody: "x"})
	}

	return items
}

func Test_DeleteMessageBatch_partial_failure(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			url := c.createQueue("q", nil)
			c.send(url, "a", nil)
			c.send(url, "b", nil)

			got := c.receive(url, map[string]any{paramMaxMessages: 2})
			require.Len(t, got, 2)

			res := c.call(opDeleteMessageBatch, map[string]any{paramQueueURL: url, paramEntries: []entry{
				{paramID: "ok-1", paramReceiptHandle: got[0].handle},
				{paramID: "bad", paramReceiptHandle: garbageHandle},
				{paramID: "ok-2", paramReceiptHandle: got[1].handle},
			}})
			require.True(t, res.ok(), "%s %s", res.errorCode(), res.errorMessage())

			assert.Equal(t, []string{"ok-1", "ok-2"}, ids(res.entries(listSuccessful, elementDeleteResult)))

			failed := res.entries(listFailed, elementFailed)
			require.Len(t, failed, 1)
			assert.Equal(t, "bad", failed[0]["Id"])
			assert.Equal(t, codeReceiptHandleInvalid, failed[0]["Code"])
			assert.Equal(t, notAValidHandle(garbageHandle), failed[0]["Message"])
			assert.Equal(t, "false", failed[0]["SenderFault"])
		})
	}
}

func Test_ChangeMessageVisibilityBatch_partial_failure(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			url := c.createQueue("q", nil)
			c.send(url, "a", nil)

			got := c.receive(url, nil)
			require.Len(t, got, 1)
			gone := c.call(opDeleteMessage, map[string]any{paramQueueURL: url, paramReceiptHandle: got[0].handle})
			require.True(t, gone.ok())

			c.send(url, "b", nil)
			live := c.receive(url, nil)
			require.Len(t, live, 1)

			res := c.call(opChangeMessageVisibilityBatch, map[string]any{paramQueueURL: url, paramEntries: []entry{
				{paramID: "live", paramReceiptHandle: live[0].handle, paramVisibilityTimeout: 60},
				{paramID: "gone", paramReceiptHandle: got[0].handle, paramVisibilityTimeout: 60},
				{paramID: "bad", paramReceiptHandle: garbageHandle, paramVisibilityTimeout: 60},
			}})
			require.True(t, res.ok(), "%s %s", res.errorCode(), res.errorMessage())

			assert.Equal(t, []string{"live"}, ids(res.entries(listSuccessful, elementChangeResult)))

			failed := res.entries(listFailed, elementFailed)
			require.Len(t, failed, 2)
			assert.Equal(t, "gone", failed[0]["Id"])
			assert.Equal(t, codeInvalidParameterException, failed[0]["Code"])
			assert.Equal(t, "Value "+got[0].handle+" for parameter ReceiptHandle is invalid. Reason: "+
				"Message does not exist or is not available for visibility timeout change.", failed[0]["Message"])
			assert.Equal(t, "bad", failed[1]["Id"])
			assert.Equal(t, codeReceiptHandleInvalid, failed[1]["Code"])
		})
	}
}

func Test_messages_are_refused_on_a_missing_queue(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			missing := "http://sqs.us-east-1.localhost.localstack.cloud:4566/000000000000/missing"

			for _, op := range []string{opSendMessage, opReceiveMessage, opDeleteMessage} {
				res := c.call(op, map[string]any{
					paramQueueURL: missing, paramMessageBody: "x", paramReceiptHandle: garbageHandle,
				})

				assert.Equal(t, http.StatusBadRequest, res.status, op)
				assert.Equal(t, errorCodes(protocol, "QueueDoesNotExist", "AWS.SimpleQueueService.NonExistentQueue"),
					res.errorCode(), op)
			}
		})
	}
}
