package sqs_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_custom_attribute_types_follow_their_family is the code-review finding on
// checkAttributeType: a custom type such as String.x skipped the checks of
// String, so a value with a character that XML 1.0 forbids was stored and then
// written, malformed, into every ReceiveMessage response.
func Test_custom_attribute_types_follow_their_family(t *testing.T) {
	const invalidContents = "Invalid characters found. Valid unicode characters are " +
		"#x9 | #xA | #xD | #x20 to #xD7FF | #xE000 to #xFFFD | #x10000 to #x10FFFF"

	const (
		customString = "String.x"
		customNumber = "Number.x"
	)

	tests := []struct {
		name        string
		give        attr
		wantMessage string
	}{
		{name: "String", give: attr{DataType: "String", StringValue: "a\x01b"}, wantMessage: invalidContents},
		{name: customString, give: attr{DataType: customString, StringValue: "a\x01b"}, wantMessage: invalidContents},
		{name: "Number", give: attr{DataType: "Number", StringValue: "1\x01"}, wantMessage: invalidContents},
		{name: customNumber, give: attr{DataType: customNumber, StringValue: "1\x01"}, wantMessage: invalidContents},
		{
			name: "String.x without a value", give: attr{DataType: customString},
			wantMessage: "Message (user) attribute 'k' must contain a non-empty value of type 'String'.",
		},
	}

	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			url := c.createQueue("q", nil)

			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					res := c.call(opSendMessage, map[string]any{
						paramQueueURL: url, paramMessageBody: "b", paramMessageAttributes: attrs{"k": tt.give},
					})

					assert.Equal(t, invalidParameterCode(protocol), res.errorCode())
					assert.Equal(t, tt.wantMessage, res.errorMessage())
				})
			}

			// Nothing was sent, and the values that are fine still are.
			assert.Empty(t, c.receive(url, nil))

			for _, give := range []attr{
				{DataType: customString, StringValue: "text"},
				{DataType: customNumber, StringValue: "12"},
				{DataType: "Binary.x", BinaryValue: []byte{1, 2}},
			} {
				c.send(url, "b", map[string]any{paramMessageAttributes: attrs{"k": give}})
			}

			assert.Len(t, c.receive(url, map[string]any{paramMaxMessages: 10}), 3)
		})
	}
}

// Test_a_received_attribute_is_well_formed_XML reads a query-protocol
// ReceiveMessage of a message whose attributes use every custom type, as XML.
func Test_a_received_attribute_is_well_formed_XML(t *testing.T) {
	c := newStack(t).as(protoQuery)
	url := c.createQueue("q", nil)

	c.send(url, "b", map[string]any{paramMessageAttributes: attrs{
		"s": {DataType: "String.x", StringValue: "a<b>&\"c\" \t\r\n"},
		"n": {DataType: "Number.x", StringValue: "1.5"},
	}})

	res := c.call(opReceiveMessage, map[string]any{paramQueueURL: url, paramMessageAttrNames: []string{attrAll}})
	require.True(t, res.ok())
	assert.NotEmpty(t, res.messages())
}
