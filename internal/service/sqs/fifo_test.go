package sqs_test

import (
	"maps"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createFIFO creates a FIFO queue. name gets the ".fifo" suffix.
func (c caller) createFIFO(extra map[string]string) string {
	c.s.t.Helper()

	attributes := map[string]string{attrFifoQueue: valueTrue}
	maps.Copy(attributes, extra)

	return c.createQueue("q.fifo", attributes)
}

// sendTo sends a message of a group, with the deduplication id when it is not "".
func (c caller) sendTo(url, body, group, dedup string) result {
	c.s.t.Helper()

	params := map[string]any{paramGroupID: group}
	if dedup != "" {
		params[paramDedupID] = dedup
	}

	return c.send(url, body, params)
}

// bodiesOf is the bodies of received messages, in order.
func bodiesOf(got []received) []string {
	bodies := make([]string, 0, len(got))
	for _, m := range got {
		bodies = append(bodies, m.body)
	}

	return bodies
}

func (c caller) deleteMessage(url, handle string) result {
	c.s.t.Helper()

	return c.call(opDeleteMessage, map[string]any{paramQueueURL: url, paramReceiptHandle: handle})
}

func (c caller) changeVisibility(url, handle string, seconds int) result {
	c.s.t.Helper()

	return c.call(opChangeMessageVisibility, map[string]any{
		paramQueueURL: url, paramReceiptHandle: handle, paramVisibilityTimeout: seconds,
	})
}

func Test_FIFO_groups_are_served_in_the_order_they_became_ready(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			url := c.createFIFO(map[string]string{attrContentDedu: valueTrue})

			for _, m := range [][2]string{
				{msgG1M1, groupOne}, {msgG2M1, groupTwo}, {msgG2M2, groupTwo}, {msgG3M1, groupThree}, {msgG1M2, groupOne},
			} {
				c.sendTo(url, m[0], m[1], "")
			}

			// A receive takes a group whole before it moves on to the next one.
			got := c.receive(url, map[string]any{paramMaxMessages: 3, paramVisibilityTimeout: 0})
			assert.Equal(t, []string{msgG1M1, msgG1M2, msgG2M1}, bodiesOf(got))

			// The groups the first receive took from queue up behind the one it
			// did not reach, in the order it gave them back.
			got = c.receive(url, map[string]any{paramMaxMessages: 10, paramVisibilityTimeout: 0})
			assert.Equal(t, []string{msgG3M1, msgG1M1, msgG1M2, msgG2M1, msgG2M2}, bodiesOf(got))
		})
	}
}

func Test_FIFO_group_is_locked_while_a_message_is_in_flight(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			url := c.createFIFO(map[string]string{attrContentDedu: valueTrue})

			c.sendTo(url, "m1", groupOne, "")
			c.sendTo(url, "m2", groupOne, "")
			c.sendTo(url, "other", groupTwo, "")

			first := c.receive(url, map[string]any{paramMaxMessages: 1})
			require.Equal(t, []string{"m1"}, bodiesOf(first))

			// The group is closed to the rest of its messages, and a new one does
			// not open it, but another group is not affected.
			c.sendTo(url, "m3", groupOne, "")
			assert.Equal(t, []string{"other"}, bodiesOf(c.receive(url, map[string]any{paramMaxMessages: 10})))
			assert.Empty(t, c.receive(url, nil))

			require.True(t, c.deleteMessage(url, first[0].handle).ok())

			// Deleting the last message in flight opens the group.
			assert.Equal(t, []string{"m2", "m3"}, bodiesOf(c.receive(url, map[string]any{paramMaxMessages: 10})))
		})
	}
}

func Test_FIFO_a_message_that_returns_opens_its_group(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			url := c.createFIFO(map[string]string{attrContentDedu: valueTrue})

			c.sendTo(url, msgG1M1, groupOne, "")
			c.sendTo(url, msgG1M2, groupOne, "")
			c.sendTo(url, msgG2M1, groupTwo, "")

			both := c.receive(url, map[string]any{paramMaxMessages: 2})
			require.Equal(t, []string{msgG1M1, msgG1M2}, bodiesOf(both))

			// Giving up the first message opens the group, which then offers the
			// messages that are not in flight: only that one.
			require.True(t, c.changeVisibility(url, both[0].handle, 0).ok())

			got := c.receive(url, map[string]any{paramMaxMessages: 3})
			assert.Equal(t, []string{msgG2M1, msgG1M1}, bodiesOf(got))
		})
	}
}

func Test_FIFO_messages_of_a_group_return_in_the_order_they_were_received(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newStack(t).as(protocol)
				url := c.createFIFO(map[string]string{attrVisibilityTimeout: "1"})

				for _, body := range []string{bodyFoo, bodyBar, bodyBaz} {
					c.sendTo(url, body, groupOne, body)
				}

				got := c.receive(url, map[string]any{paramMaxMessages: 5})
				require.Equal(t, []string{bodyFoo, bodyBar, bodyBaz}, bodiesOf(got))

				// bar keeps its hold for longer: baz, whose time is over, waits behind it.
				require.True(t, c.changeVisibility(url, got[1].handle, 4).ok())

				time.Sleep(2500 * time.Millisecond)

				back := c.receive(url, map[string]any{paramMaxMessages: 5, paramVisibilityTimeout: 30})
				require.Equal(t, []string{bodyFoo}, bodiesOf(back))
				require.True(t, c.deleteMessage(url, back[0].handle).ok())

				time.Sleep(time.Second)
				assert.Empty(t, c.receive(url, nil), "bar still holds its group's messages")

				time.Sleep(time.Second)
				assert.Equal(t, []string{bodyBar, bodyBaz}, bodiesOf(c.receive(url, map[string]any{paramMaxMessages: 5})))
			})
		})
	}
}

func Test_FIFO_long_poll_wakes_when_a_delete_opens_a_group(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newStack(t).as(protoJSON)
		url := c.createFIFO(map[string]string{attrContentDedu: valueTrue})

		c.sendTo(url, "m1", groupOne, "")
		c.sendTo(url, "m2", groupOne, "")

		first := c.receive(url, map[string]any{paramMaxMessages: 1})
		require.Len(t, first, 1)

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
		require.True(t, c.deleteMessage(url, first[0].handle).ok())
		<-done

		assert.Equal(t, []string{"m2"}, bodiesOf(got))
		assert.Equal(t, 3*time.Second, elapsed)
	})
}

func Test_FIFO_deduplication(t *testing.T) {
	tests := []struct {
		name      string
		giveQueue map[string]string
		giveFirst [3]string // body, group, deduplication id
		giveNext  [3]string
		wantSame  bool
	}{
		{
			name: "the same body", giveQueue: map[string]string{attrContentDedu: valueTrue},
			giveFirst: [3]string{"x", groupOne, ""}, giveNext: [3]string{"x", groupOne, ""}, wantSame: true,
		},
		{
			name: "another body", giveQueue: map[string]string{attrContentDedu: valueTrue},
			giveFirst: [3]string{"x", groupOne, ""}, giveNext: [3]string{"y", groupOne, ""},
		},
		{
			name: "the same id and another body", giveFirst: [3]string{"x", groupOne, "d"},
			giveNext: [3]string{"y", groupOne, "d"}, wantSame: true,
		},
		{
			name: "the same id in another group", giveFirst: [3]string{"x", groupOne, "d"},
			giveNext: [3]string{"x", groupTwo, "d"}, wantSame: true,
		},
		{
			name:      "the same id in another group of a messageGroup scope",
			giveQueue: map[string]string{attrDedupScope: "messageGroup"}, giveFirst: [3]string{"x", groupOne, "d"},
			giveNext: [3]string{"x", groupTwo, "d"},
		},
		{
			name:      "the same id in the same group of a messageGroup scope",
			giveQueue: map[string]string{attrDedupScope: "messageGroup"}, giveFirst: [3]string{"x", groupOne, "d"},
			giveNext: [3]string{"x", groupOne, "d"}, wantSame: true,
		},
		{
			name: "an explicit id wins over the body", giveQueue: map[string]string{attrContentDedu: valueTrue},
			giveFirst: [3]string{"x", groupOne, "d"}, giveNext: [3]string{"x", groupOne, "e"},
		},
	}

	for _, protocol := range protocols {
		for _, tt := range tests {
			t.Run(protocol+"/"+tt.name, func(t *testing.T) {
				c := newStack(t).as(protocol)
				url := c.createFIFO(tt.giveQueue)

				first := c.sendTo(url, tt.giveFirst[0], tt.giveFirst[1], tt.giveFirst[2])
				// The first message is gone before the next is sent: a deleted message
				// is still remembered.
				got := c.receive(url, nil)
				require.Len(t, got, 1)
				require.True(t, c.deleteMessage(url, got[0].handle).ok())

				next := c.sendTo(url, tt.giveNext[0], tt.giveNext[1], tt.giveNext[2])
				left := c.receive(url, nil)

				if tt.wantSame {
					assert.Equal(t, first.str("MessageId"), next.str("MessageId"), "the sender hears of the first message")
					assert.Equal(t, first.str(paramSequence), next.str(paramSequence))
					assert.Empty(t, left, "a duplicate is not queued")

					return
				}

				assert.NotEqual(t, first.str("MessageId"), next.str("MessageId"))
				assert.Len(t, left, 1)
			})
		}
	}
}

func Test_FIFO_deduplication_window(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newStack(t).as(protocol)
				url := c.createFIFO(nil)

				first := c.sendTo(url, "a", groupOne, "d")

				time.Sleep(5*minute - time.Second)

				within := c.sendTo(url, "b", groupOne, "d")
				assert.Equal(t, first.str("MessageId"), within.str("MessageId"), "just inside the window")

				got := c.receive(url, nil)
				require.Equal(t, []string{"a"}, bodiesOf(got))
				require.True(t, c.deleteMessage(url, got[0].handle).ok())
				assert.Empty(t, c.receive(url, nil), "the duplicate was not queued")

				time.Sleep(time.Second)

				after := c.sendTo(url, "c", groupOne, "d")
				assert.NotEqual(t, first.str("MessageId"), after.str("MessageId"), "the window is over")
				assert.Equal(t, []string{"c"}, bodiesOf(c.receive(url, nil)))
			})
		})
	}
}

func Test_FIFO_purge_forgets_deduplication_ids(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			url := c.createFIFO(nil)

			c.sendTo(url, "a", groupOne, "d")
			require.True(t, c.call(opPurgeQueue, map[string]any{paramQueueURL: url}).ok())

			c.sendTo(url, "a", groupOne, "d")
			assert.Equal(t, []string{"a"}, bodiesOf(c.receive(url, nil)))
		})
	}
}

func Test_FIFO_send_errors(t *testing.T) {
	tests := []struct {
		name        string
		giveQueue   map[string]string
		giveParams  map[string]any
		wantCode    [2]string // json and query
		wantMessage string
	}{
		{
			name: "no group", giveParams: map[string]any{paramDedupID: "d"},
			wantCode:    [2]string{"MissingRequiredParameterException", "MissingParameter"},
			wantMessage: "The request must contain the parameter MessageGroupId.",
		},
		{
			name: "no deduplication id", giveParams: map[string]any{paramGroupID: groupOne},
			wantCode: [2]string{codeInvalidParameterException, codeInvalidParameter},
			wantMessage: "The queue should either have ContentBasedDeduplication enabled or " +
				"MessageDeduplicationId provided explicitly",
		},
		{
			name: "no deduplication id and the queue does not make one", giveQueue: map[string]string{attrContentDedu: "False"},
			giveParams: map[string]any{paramGroupID: groupOne},
			wantCode:   [2]string{codeInvalidParameterException, codeInvalidParameter},
			wantMessage: "The queue should either have ContentBasedDeduplication enabled or " +
				"MessageDeduplicationId provided explicitly",
		},
		{
			name: "a delay", giveParams: map[string]any{paramGroupID: groupOne, paramDedupID: "d", paramDelaySeconds: 2},
			wantCode: [2]string{codeInvalidParameterException, codeInvalidParameter},
			wantMessage: "Value 2 for parameter DelaySeconds is invalid. " +
				"Reason: The request include parameter that is not valid for this queue type.",
		},
		{
			name: "a group id of spaces", giveParams: map[string]any{paramGroupID: "group 1", paramDedupID: "d"},
			wantCode: [2]string{codeInvalidParameterException, codeInvalidParameter},
			wantMessage: "Value group 1 for parameter MessageGroupId is invalid. Reason: MessageGroupId can only " +
				"include alphanumeric and punctuation characters. 1 to 128 in length.",
		},
	}

	for _, protocol := range protocols {
		for _, tt := range tests {
			t.Run(protocol+"/"+tt.name, func(t *testing.T) {
				c := newStack(t).as(protocol)
				url := c.createFIFO(tt.giveQueue)

				res := c.send2(url, "body", tt.giveParams)

				assert.Equal(t, errorCodes(protocol, tt.wantCode[0], tt.wantCode[1]), res.errorCode())
				assert.Equal(t, tt.wantMessage, res.errorMessage())
			})
		}
	}
}

// send2 sends a message and returns the result whether or not it failed.
func (c caller) send2(url, body string, params map[string]any) result {
	c.s.t.Helper()

	all := map[string]any{paramQueueURL: url, paramMessageBody: body}
	maps.Copy(all, params)

	return c.call(opSendMessage, all)
}

func Test_FIFO_a_zero_delay_on_the_message_means_the_queue_delay(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newStack(t).as(protocol)
				url := c.createFIFO(map[string]string{attrDelaySeconds: "2", attrContentDedu: valueTrue})

				c.send(url, "a", map[string]any{paramGroupID: groupOne, paramDelaySeconds: 0})

				assert.Empty(t, c.receive(url, nil))

				time.Sleep(2 * time.Second)
				assert.Equal(t, []string{"a"}, bodiesOf(c.receive(url, nil)))
			})
		})
	}
}

func Test_FIFO_sequence_numbers_grow(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			url := c.createFIFO(nil)

			var last uint64

			for i := range 3 {
				sent := c.sendTo(url, "m", groupOne, "d"+strconv.Itoa(i))

				number, err := strconv.ParseUint(sent.str(paramSequence), 10, 64)
				require.NoError(t, err)
				assert.Greater(t, number, last)

				last = number
			}

			got := c.receive(url, map[string]any{paramSystemAttrNames: []string{attrAll}, paramMaxMessages: 1})
			require.Len(t, got, 1)
			assert.NotEmpty(t, got[0].system[paramSequence])
			assert.Equal(t, groupOne, got[0].system[paramGroupID])
			assert.Equal(t, "d0", got[0].system[paramDedupID])

			standard := c.createQueue("standard", nil)
			assert.Empty(t, c.send(standard, "m", nil).str(paramSequence), "a standard queue has none")
		})
	}
}

func Test_FIFO_batches(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			url := c.createFIFO(nil)

			sendBatch := func(entries ...map[string]any) result {
				return c.call(opSendMessageBatch, map[string]any{paramQueueURL: url, paramEntries: entries})
			}

			noGroup := sendBatch(
				entry{paramID: "a", paramMessageBody: "x", paramGroupID: groupOne, paramDedupID: "a"},
				entry{paramID: "b", paramMessageBody: "y", paramDedupID: "b"},
			)
			assert.Equal(t, invalidParameterCode(protocol), noGroup.errorCode(), "the whole batch is refused")
			assert.Equal(t, "The request must contain the parameter MessageGroupId.", noGroup.errorMessage())

			noDedup := sendBatch(
				entry{paramID: "a", paramMessageBody: "x", paramGroupID: groupOne, paramDedupID: "a"},
				entry{paramID: "b", paramMessageBody: "y", paramGroupID: groupOne},
			)
			assert.Equal(t, invalidParameterCode(protocol), noDedup.errorCode())

			sent := sendBatch(
				entry{paramID: "a", paramMessageBody: "x", paramGroupID: groupOne, paramDedupID: "a"},
				entry{paramID: "b", paramMessageBody: "y", paramGroupID: groupOne, paramDedupID: "b"},
				entry{paramID: "c", paramMessageBody: "z", paramGroupID: "bad group", paramDedupID: "c"},
			)
			require.True(t, sent.ok())

			ok := sent.entries(listSuccessful, elementSendResult)
			require.Len(t, ok, 2)
			assert.Less(t, ok[0][paramSequence], ok[1][paramSequence])
			assert.Equal(t, []string{"c"}, ids(sent.entries(listFailed, elementFailed)))

			assert.Equal(t, []string{"x", "y"}, bodiesOf(c.receive(url, map[string]any{paramMaxMessages: 10})))
		})
	}
}

func Test_FIFO_a_handle_that_expired_cannot_delete(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newStack(t).as(protocol)
				url := c.createFIFO(map[string]string{attrVisibilityTimeout: "1", attrContentDedu: valueTrue})

				c.sendTo(url, "a", groupOne, "")
				got := c.receive(url, nil)
				require.Len(t, got, 1)

				time.Sleep(time.Second)

				res := c.deleteMessage(url, got[0].handle)
				assert.Equal(t, invalidParameterCode(protocol), res.errorCode())
				assert.Equal(t, "Value "+got[0].handle+" for parameter ReceiptHandle is invalid. "+
					"Reason: The receipt handle has expired.", res.errorMessage())

				// The message is back, and a receive hands it out with a handle that works.
				again := c.receive(url, map[string]any{paramVisibilityTimeout: 10})
				require.Len(t, again, 1)
				assert.True(t, c.deleteMessage(url, again[0].handle).ok())
			})
		})
	}
}

func Test_FIFO_counts(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			url := c.createFIFO(map[string]string{attrContentDedu: valueTrue})

			for _, m := range [][2]string{
				{msgG1M1, groupOne}, {msgG1M2, groupOne}, {msgG1M3, groupOne}, {msgG2M1, groupTwo}, {msgG3M1, groupThree},
			} {
				c.sendTo(url, m[0], m[1], "")
			}

			counts := func() map[string]string {
				res := c.call(opGetQueueAttributes, map[string]any{paramQueueURL: url, paramAttributeNames: []string{attrAll}})

				return res.attributes()
			}

			assert.Equal(t, "5", counts()["ApproximateNumberOfMessages"])

			got := c.receive(url, map[string]any{paramMaxMessages: 4})
			require.Len(t, got, 4)

			// What waits in a group that is closed still counts as visible.
			assert.Equal(t, "1", counts()["ApproximateNumberOfMessages"])
			assert.Equal(t, "4", counts()["ApproximateNumberOfMessagesNotVisible"])
		})
	}
}

func Test_a_standard_queue_takes_a_message_group_id(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			url := c.createQueue("q", nil)

			c.send(url, "a", map[string]any{paramGroupID: "test"})

			got := c.receive(url, map[string]any{paramSystemAttrNames: []string{attrAll}})
			require.Len(t, got, 1)
			assert.Equal(t, "test", got[0].system[paramGroupID])

			bad := c.send2(url, "b", map[string]any{paramGroupID: "group 123"})
			assert.Equal(t, invalidParameterCode(protocol), bad.errorCode())

			dedup := c.send2(url, "c", map[string]any{paramDedupID: "d"})
			assert.Equal(t, invalidParameterCode(protocol), dedup.errorCode())
		})
	}
}
