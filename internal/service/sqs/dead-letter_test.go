package sqs_test

import (
	"encoding/json"
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	opListDeadLetterSources = "ListDeadLetterSourceQueues"
	sysDeadLetterSource     = "DeadLetterQueueSourceArn"
	sysReceiveCount         = "ApproximateReceiveCount"
)

// arnOf returns the ARN of a queue.
func (c caller) arnOf(url string) string {
	c.s.t.Helper()

	res := c.call(opGetQueueAttributes, map[string]any{paramQueueURL: url, paramAttributeNames: []string{attrQueueArn}})
	require.True(c.s.t, res.ok())

	return res.attributes()[attrQueueArn]
}

// redrivePolicy is the RedrivePolicy attribute of a queue that sends to target
// after max receives.
func redrivePolicy(t *testing.T, target string, maxReceives int) string {
	t.Helper()

	policy, err := json.Marshal(map[string]any{"deadLetterTargetArn": target, "maxReceiveCount": maxReceives})
	require.NoError(t, err)

	return string(policy)
}

// receiveAll receives with all system attributes, leaving the messages visible.
func (c caller) receiveAll(url string, extra map[string]any) []received {
	c.s.t.Helper()

	params := map[string]any{paramSystemAttrNames: []string{attrAll}, paramVisibilityTimeout: 0}
	maps.Copy(params, extra)

	return c.receive(url, params)
}

func Test_a_message_moves_to_the_dead_letter_queue_after_maxReceiveCount(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			dead := c.createQueue("dead", nil)
			source := c.createQueue("source", map[string]string{
				attrRedrivePolicy: redrivePolicy(t, c.arnOf(dead), 2),
			})

			sent := c.send(source, "poison", nil)

			for range 2 {
				got := c.receiveAll(source, nil)
				require.Len(t, got, 1)
			}

			// The third receive finds the message past its limit and passes it on.
			assert.Empty(t, c.receiveAll(source, nil))
			assert.Equal(t, "0", attributesOf(c, source)["ApproximateNumberOfMessages"])

			moved := c.receiveAll(dead, nil)
			require.Len(t, moved, 1)
			assert.Equal(t, sent.str("MessageId"), moved[0].id)
			assert.Equal(t, "poison", moved[0].body)
			assert.Equal(t, bodyMD5("poison"), moved[0].bodyMD5)
			assert.Equal(t, c.arnOf(source), moved[0].system[sysDeadLetterSource])
			assert.Equal(t, "3", moved[0].system[sysReceiveCount], "the receives so far count on")
		})
	}
}

func attributesOf(c caller, url string) map[string]string {
	res := c.call(opGetQueueAttributes, map[string]any{paramQueueURL: url, paramAttributeNames: []string{attrAll}})

	return res.attributes()
}

func Test_a_dead_letter_does_not_use_up_a_slot_of_the_receive(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			dead := c.createQueue("dead", nil)
			source := c.createQueue("source", map[string]string{
				attrRedrivePolicy: redrivePolicy(t, c.arnOf(dead), 1),
			})

			c.send(source, "old", nil)
			require.Len(t, c.receiveAll(source, nil), 1)

			c.send(source, "new", nil)

			got := c.receiveAll(source, map[string]any{paramMaxMessages: 1})
			require.Len(t, got, 1)
			assert.Equal(t, "new", got[0].body)
			assert.Equal(t, []string{"old"}, bodiesOf(c.receiveAll(dead, nil)))
		})
	}
}

func Test_a_FIFO_message_moves_to_a_FIFO_dead_letter_queue(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			dead := c.createFIFO(map[string]string{attrContentDedu: valueTrue})

			// The dead-letter queue is the queue "q.fifo" of createFIFO, so the
			// source needs a name of its own.
			source := c.createQueue("source.fifo", map[string]string{
				attrFifoQueue:     valueTrue,
				attrContentDedu:   valueTrue,
				attrRedrivePolicy: redrivePolicy(t, c.arnOf(dead), 2),
			})

			first := c.sendTo(source, "foobar", "1", "")
			for range 2 {
				require.Len(t, c.receiveAll(source, nil), 1)
			}

			assert.Empty(t, c.receiveAll(source, nil))

			moved := c.receiveAll(dead, nil)
			require.Len(t, moved, 1)
			assert.Equal(t, first.str("MessageId"), moved[0].id)
			assert.Equal(t, "1", moved[0].system[paramGroupID])
			assert.NotEmpty(t, moved[0].system[paramSequence])

			// The group of the source is open again.
			second := c.sendTo(source, "foobar2", "1", "")
			got := c.receiveAll(source, nil)
			require.Len(t, got, 1)
			assert.Equal(t, second.str("MessageId"), got[0].id)
		})
	}
}

// Test_a_missing_dead_letter_queue_loses_no_message is the code-review finding
// on dead-lettering: the message used to leave the source before the put, so a
// dead-letter queue that was gone took it with it. LocalStack fails the receive
// and drops the message; here it stays on the source and is received as an
// ordinary one, until the dead-letter queue is back.
func Test_a_missing_dead_letter_queue_loses_no_message(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			dead := c.createQueue("dead", nil)
			deadARN := c.arnOf(dead)
			source := c.createQueue("source", map[string]string{
				attrRedrivePolicy: redrivePolicy(t, deadARN, 1),
			})

			sent := c.send(source, "m", nil)
			require.Len(t, c.receiveAll(source, nil), 1)
			require.True(t, c.call(opDeleteQueue, map[string]any{paramQueueURL: dead}).ok())

			// The second receive finds the message past its limit and cannot move it.
			got := c.receiveAll(source, nil)
			require.Len(t, got, 1, "the message is still received")
			assert.Equal(t, sent.str("MessageId"), got[0].id)
			assert.Equal(t, "2", got[0].system[sysReceiveCount])
			assert.NotEmpty(t, got[0].handle)

			// Once the dead-letter queue is back the next receive moves it.
			c.createQueue("dead", nil)
			assert.Empty(t, c.receiveAll(source, nil))
			assert.Equal(t, []string{"m"}, bodiesOf(c.receiveAll(c.queueURL("dead"), nil)))
			assert.Equal(t, "0", attributesOf(c, source)["ApproximateNumberOfMessages"])
		})
	}
}

// Test_a_failed_dead_letter_hides_nothing_unreceived is the finding on poll: the
// messages of a receive whose dead letters failed were hidden for the visibility
// timeout and never handed out.
func Test_a_failed_dead_letter_hides_nothing_unreceived(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			dead := c.createQueue("dead", nil)
			source := c.createQueue("source", map[string]string{
				attrRedrivePolicy: redrivePolicy(t, c.arnOf(dead), 1),
			})

			c.send(source, "old", nil)
			require.Len(t, c.receiveAll(source, nil), 1)
			c.send(source, "new", nil)
			require.True(t, c.call(opDeleteQueue, map[string]any{paramQueueURL: dead}).ok())

			// Every message that the receive hides, it hands out.
			got := c.receive(source, map[string]any{paramMaxMessages: 10, paramVisibilityTimeout: 30})
			assert.ElementsMatch(t, []string{"old", "new"}, bodiesOf(got))

			counts := attributesOf(c, source)
			assert.Equal(t, "0", counts["ApproximateNumberOfMessages"])
			assert.Equal(t, "2", counts["ApproximateNumberOfMessagesNotVisible"])

			for _, m := range got {
				assert.True(t, c.deleteMessage(source, m.handle).ok(), "the handle of %s is good", m.body)
			}
		})
	}
}

// Test_a_failed_dead_letter_over_the_receive_size_stays_visible covers the
// messages of a failed dead-letter move that the response has no room for: they
// stay visible, not hidden under a handle that nobody gets.
func Test_a_failed_dead_letter_over_the_receive_size_stays_visible(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			dead := c.createQueue("dead", nil)
			source := c.createQueue("source", map[string]string{
				attrRedrivePolicy: redrivePolicy(t, c.arnOf(dead), 1),
			})

			c.send(source, "a", nil)
			c.send(source, "b", nil)
			require.Len(t, c.receiveAll(source, map[string]any{paramMaxMessages: 2}), 2)
			require.True(t, c.call(opDeleteQueue, map[string]any{paramQueueURL: dead}).ok())

			got := c.receive(source, map[string]any{paramMaxMessages: 1, paramVisibilityTimeout: 30})
			require.Len(t, got, 1)

			counts := attributesOf(c, source)
			assert.Equal(t, "1", counts["ApproximateNumberOfMessages"], "the other one is visible")
			assert.Equal(t, "1", counts["ApproximateNumberOfMessagesNotVisible"])
		})
	}
}

func Test_a_missing_dead_letter_queue_loses_no_FIFO_message(t *testing.T) {
	c := newStack(t).as(protoJSON)
	dead := c.createQueue("dead.fifo", map[string]string{attrFifoQueue: valueTrue, attrContentDedu: valueTrue})
	source := c.createQueue("source.fifo", map[string]string{
		attrFifoQueue:     valueTrue,
		attrContentDedu:   valueTrue,
		attrRedrivePolicy: redrivePolicy(t, c.arnOf(dead), 1),
	})

	sent := c.sendTo(source, "first", "g", "")
	c.sendTo(source, "second", "g", "")
	require.Len(t, c.receiveAll(source, nil), 1)
	require.True(t, c.call(opDeleteQueue, map[string]any{paramQueueURL: dead}).ok())

	got := c.receiveAll(source, map[string]any{paramMaxMessages: 10, paramVisibilityTimeout: 30})
	require.Len(t, got, 2)
	assert.Equal(t, sent.str("MessageId"), got[0].id, "the message that could not move is still first")

	for _, m := range got {
		assert.True(t, c.deleteMessage(source, m.handle).ok(), "the handle of %s is good", m.body)
	}
}

func (c caller) queueURL(name string) string {
	c.s.t.Helper()

	res := c.call("GetQueueUrl", map[string]any{paramQueueName: name})
	require.True(c.s.t, res.ok())

	return res.str(paramQueueURL)
}

// Test_CreateQueue_checks_the_RedrivePolicy is the finding that CreateQueue took
// a policy that SetQueueAttributes refuses. Whether the target exists is checked
// by neither, as in LocalStack.
func Test_CreateQueue_checks_the_RedrivePolicy(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)

			for name, policy := range map[string]string{
				"no target": `{"maxReceiveCount":3}`,
				"no count":  `{"deadLetterTargetArn":"arn:aws:sqs:us-east-1:000000000000:gone"}`,
				"zero":      `{"deadLetterTargetArn":"arn:aws:sqs:us-east-1:000000000000:gone","maxReceiveCount":0}`,
			} {
				res := c.call("CreateQueue", map[string]any{
					paramQueueName: "refused", paramAttributes: map[string]string{attrRedrivePolicy: policy},
				})
				assert.Equal(t, invalidParameterCode(protocol), res.errorCode(), name)
			}

			assert.Empty(t, c.listQueues(), "no queue was made")

			ok := c.call("CreateQueue", map[string]any{
				paramQueueName: "accepted",
				paramAttributes: map[string]string{
					attrRedrivePolicy: `{"deadLetterTargetArn":"arn:aws:sqs:us-east-1:000000000000:gone","maxReceiveCount":3}`,
				},
			})
			assert.True(t, ok.ok(), "a target that does not exist yet is fine")
		})
	}
}

func Test_ListDeadLetterSourceQueues(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			dead := c.createQueue("dead", nil)
			other := c.createQueue("other-dead", nil)
			policy := redrivePolicy(t, c.arnOf(dead), 5)

			first := c.createQueue("first", map[string]string{attrRedrivePolicy: policy})
			second := c.createQueue("second", map[string]string{attrRedrivePolicy: policy})
			c.createQueue("elsewhere", map[string]string{attrRedrivePolicy: redrivePolicy(t, c.arnOf(other), 5)})
			c.createQueue("plain", nil)

			res := c.call(opListDeadLetterSources, map[string]any{paramQueueURL: dead})
			require.True(t, res.ok())
			assert.Equal(t, []string{first, second}, sourceURLs(res))

			none := c.call(opListDeadLetterSources, map[string]any{paramQueueURL: first})
			require.True(t, none.ok())
			assert.Empty(t, sourceURLs(none))

			// Removing the policy takes the queue off the list.
			set := c.call(opSetAttributes, map[string]any{
				paramQueueURL: first, paramAttributes: map[string]string{attrRedrivePolicy: ""},
			})
			require.True(t, set.ok())
			assert.Equal(t, []string{second}, sourceURLs(c.call(opListDeadLetterSources, map[string]any{paramQueueURL: dead})))
		})
	}
}

// sourceURLs reads the queue URLs of a ListDeadLetterSourceQueues result. The
// json member is "queueUrls", which the helper for ListQueues does not read.
func sourceURLs(r result) []string {
	if r.query {
		return r.urls()
	}

	items, _ := r.json["queueUrls"].([]any)
	urls := make([]string, 0, len(items))

	for _, item := range items {
		text, _ := item.(string)
		urls = append(urls, text)
	}

	return urls
}

func Test_RedriveAllowPolicy_validation(t *testing.T) {
	tests := []struct {
		name     string
		givePol  string
		wantOK   bool
		wantKept bool
	}{
		{name: "allowAll", givePol: `{"redrivePermission":"allowAll"}`, wantOK: true, wantKept: true},
		{name: "denyAll", givePol: `{"redrivePermission":"denyAll"}`, wantOK: true, wantKept: true},
		{
			name:    "byQueue with sources",
			givePol: `{"redrivePermission":"byQueue","sourceQueueArns":["arn:aws:sqs:us-east-1:000000000000:a"]}`,
			wantOK:  true, wantKept: true,
		},
		{name: "byQueue without sources", givePol: `{"redrivePermission":"byQueue"}`},
		{name: "an unknown permission", givePol: `{"redrivePermission":"sometimes"}`},
		{name: "not json", givePol: "allowAll"},
		{name: "empty means none", givePol: "", wantOK: true},
	}

	for _, protocol := range protocols {
		for _, tt := range tests {
			t.Run(protocol+"/"+tt.name, func(t *testing.T) {
				c := newStack(t).as(protocol)
				url := c.createQueue("q", nil)

				res := c.call(opSetAttributes, map[string]any{
					paramQueueURL: url, paramAttributes: map[string]string{attrRedriveAllowPolicy: tt.givePol},
				})

				assert.Equal(t, tt.wantOK, res.ok())

				if !tt.wantOK {
					assert.Equal(t, "InvalidAttributeValue", res.errorCode())
				}

				_, kept := attributesOf(c, url)[attrRedriveAllowPolicy]
				assert.Equal(t, tt.wantKept, kept)
			})
		}
	}
}

func Test_a_changed_RedrivePolicy_applies_to_the_next_receive(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			dead := c.createQueue("dead", nil)
			source := c.createQueue("source", map[string]string{
				attrRedrivePolicy: redrivePolicy(t, c.arnOf(dead), 1),
			})

			c.send(source, "m", nil)
			require.Len(t, c.receiveAll(source, nil), 1)

			set := c.call(opSetAttributes, map[string]any{
				paramQueueURL: source, paramAttributes: map[string]string{attrRedrivePolicy: ""},
			})
			require.True(t, set.ok())

			assert.Len(t, c.receiveAll(source, nil), 1, "no policy, no limit")
			assert.Empty(t, c.receiveAll(dead, nil))
		})
	}
}
