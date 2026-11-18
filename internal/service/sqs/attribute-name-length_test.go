package sqs_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_a_message_attribute_name_can_have_256_characters is SQ-66. AWS documents
// "up to 256 characters" for a name. LocalStack 8b9a79f refuses 256 (probed in
// verification/SQ-145/ls-probe.out); gostack takes it and refuses 257. A name
// with Latin letters outside ASCII still counts by characters and still passes,
// as the validated test_send_message_with_invalid_string_attributes expects.
func Test_a_message_attribute_name_can_have_256_characters(t *testing.T) {
	t.Parallel()

	const tooLong = "Message (user) attribute names must be shorter than 256 Bytes"

	tests := []struct {
		name     string
		giveName string
		wantOK   bool
	}{
		{"255 characters", strings.Repeat("L", 255), true},
		{"256 characters", strings.Repeat("L", 256), true},
		{"257 characters", strings.Repeat("L", 257), false},
		{"256 non-ASCII characters", strings.Repeat("ä", 256), true},
		{"257 non-ASCII characters", strings.Repeat("ä", 257), false},
		{"a short non-ASCII name", "attr.1øßä", true},
	}

	for _, protocol := range protocols {
		for _, tt := range tests {
			t.Run(protocol+" "+tt.name, func(t *testing.T) {
				t.Parallel()

				c := newStack(t).as(protocol)
				queueURL := c.createQueue("q", nil)

				res := c.call("SendMessage", map[string]any{
					paramQueueURL:          queueURL,
					paramMessageBody:       "b",
					paramMessageAttributes: attrs{tt.giveName: {DataType: typeString, StringValue: "v"}},
				})

				if tt.wantOK {
					require.True(t, res.ok(), "%s %s", res.errorCode(), res.errorMessage())

					return
				}

				assert.Equal(t, 400, res.status)
				assert.Equal(t, tooLong, res.errorMessage())
			})
		}
	}
}

// Test_a_batch_entry_attribute_name_can_have_256_characters is the same limit in
// SendMessageBatch, where a refusal is a Failed entry. A batch entry Id is
// unchanged: 80 characters at most.
func Test_a_batch_entry_attribute_name_can_have_256_characters(t *testing.T) {
	t.Parallel()

	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			t.Parallel()

			c := newStack(t).as(protocol)
			queueURL := c.createQueue("q", nil)

			res := c.call("SendMessageBatch", map[string]any{
				paramQueueURL: queueURL,
				"Entries": []entry{
					{paramID: "ok", paramMessageBody: "b", paramMessageAttributes: attrs{
						strings.Repeat("L", 256): {DataType: typeString, StringValue: "v"},
					}},
					{paramID: "long", paramMessageBody: "b", paramMessageAttributes: attrs{
						strings.Repeat("L", 257): {DataType: typeString, StringValue: "v"},
					}},
				},
			})

			require.True(t, res.ok(), "%s %s", res.errorCode(), res.errorMessage())
			assert.Equal(t, []string{"ok"}, ids(res.entries("Successful", "SendMessageBatchResultEntry")))
			assert.Equal(t, []string{"long"}, ids(res.entries("Failed", "BatchResultErrorEntry")))

			res = c.call("SendMessageBatch", map[string]any{
				paramQueueURL: queueURL,
				"Entries":     []entry{{paramID: strings.Repeat("i", 81), paramMessageBody: "b"}},
			})

			require.Equal(t, 400, res.status, "an entry id of 81 characters fails the whole batch")
			assert.Contains(t, res.errorCode(), "InvalidBatchEntryId")
		})
	}
}
