package sqs_test

import (
	"crypto/md5"
	"encoding/hex"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	traceHeaderValue = "Root=1-5759e988-bd862e3fe1be46a994272793;Parent=53995c3f42cd8ad8;Sampled=1"
	// md5KX is the MD5OfMessageAttributes of {"k": String "x"}, from LocalStack's
	// snapshot of test_send_message_batch_with_oversized_contents.
	md5KX = "a45daa70926828a2f0a1db3418e6feb4"
	// md5Trace is the MD5OfMessageSystemAttributes of an AWSTraceHeader, from
	// test_system_attributes_have_no_effect_on_attr_md5.
	md5Trace = "5ae4d5d7636402d80f4eb6d213245a88"
	// md5Binary is the MD5OfMessageAttributes of the binary attribute of
	// test_send_message_with_binary_attributes.
	md5Binary = "8cbe4db156db8a94db8b801b7addb984"
	// md5Filters is the one of test_receive_message_message_attribute_names_filters.
	md5Filters = "4c360f3fdafd970e05fae2f149d997f5"
)

func bodyMD5(body string) string {
	sum := md5.Sum([]byte(body))

	return hex.EncodeToString(sum[:])
}

// errorCodes is the code of an error as each protocol shows it.
func errorCodes(protocol, jsonCode, queryCode string) string {
	if protocol == protoQuery {
		return queryCode
	}

	return jsonCode
}

func Test_SendMessage_and_ReceiveMessage(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newStack(t).as(protocol)
				url := c.createQueue("q", nil)

				sent := c.send(url, "hello", map[string]any{
					paramMessageAttributes:  attrs{"k": {DataType: typeString, StringValue: "x"}},
					paramMessageSystemAttrs: attrs{sysTraceHeader: {DataType: typeString, StringValue: traceHeaderValue}},
				})

				assert.Equal(t, bodyMD5("hello"), sent.str("MD5OfMessageBody"))
				assert.Equal(t, md5KX, sent.str("MD5OfMessageAttributes"))
				assert.Equal(t, md5Trace, sent.str("MD5OfMessageSystemAttributes"))
				assert.NotEmpty(t, sent.str("MessageId"))

				sentAt := time.Now()

				got := c.receive(url, map[string]any{
					paramAttributeNames:   []string{attrAll},
					paramMessageAttrNames: []string{attrAll},
				})
				require.Len(t, got, 1)

				m := got[0]
				assert.Equal(t, sent.str("MessageId"), m.id)
				assert.Equal(t, "hello", m.body)
				assert.Equal(t, bodyMD5("hello"), m.bodyMD5)
				assert.Equal(t, md5KX, m.attrsMD5)
				assert.Equal(t, attrs{"k": {DataType: typeString, StringValue: "x"}}, m.userAttrs)
				assert.Equal(t, map[string]string{
					sysSenderID:                        defaultAccount,
					sysSentTimestamp:                   strconv.FormatInt(sentAt.UnixMilli(), 10),
					sysTraceHeader:                     traceHeaderValue,
					"ApproximateReceiveCount":          "1",
					"ApproximateFirstReceiveTimestamp": strconv.FormatInt(sentAt.UnixMilli(), 10),
				}, m.system)
				assert.NotEmpty(t, m.handle)

				// The message is hidden for the queue's visibility timeout of 30 seconds,
				// and comes back with a higher receive count and the same first receive.
				assert.Empty(t, c.receive(url, nil))

				time.Sleep(30 * time.Second)

				again := c.receive(url, map[string]any{paramAttributeNames: []string{attrAll}})
				require.Len(t, again, 1)
				assert.Equal(t, m.id, again[0].id)
				assert.NotEqual(t, m.handle, again[0].handle)
				assert.Equal(t, "2", again[0].system["ApproximateReceiveCount"])
				assert.Equal(t, m.system["ApproximateFirstReceiveTimestamp"], again[0].system["ApproximateFirstReceiveTimestamp"])
			})
		})
	}
}

func Test_MD5_of_binary_attributes(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			url := c.createQueue("q", nil)

			binary := []byte("traceparent\x1e00-774062d6c37081a5a0b9b5b88e30627c-2d2482211f6489da-01")
			sent := c.send(url, "test", map[string]any{
				paramMessageAttributes: attrs{"attr1": {DataType: "Binary", BinaryValue: binary}},
			})
			assert.Equal(t, md5Binary, sent.str("MD5OfMessageAttributes"))

			got := c.receive(url, map[string]any{paramMessageAttrNames: []string{attrAll}})
			require.Len(t, got, 1)
			assert.Equal(t, md5Binary, got[0].attrsMD5)
			assert.Equal(t, attrs{"attr1": {DataType: "Binary", BinaryValue: binary}}, got[0].userAttrs)
		})
	}
}

func Test_ReceiveMessage_attribute_filters(t *testing.T) {
	everyAttribute := []string{attrNameGeneral, attrNameHello, attrNameHelpMe}

	tests := []struct {
		name           string
		giveAttrNames  []string
		giveSystem     []string
		wantAttributes []string
		wantSystem     []string
	}{
		{name: "no filter", wantAttributes: nil, wantSystem: nil},
		{name: "All", giveAttrNames: []string{attrAll}, wantAttributes: everyAttribute},
		{name: "dot star", giveAttrNames: []string{".*"}, wantAttributes: everyAttribute},
		{name: "star", giveAttrNames: []string{"*"}, wantAttributes: everyAttribute},
		{name: "names that do not exist", giveAttrNames: []string{"Foo", "Help"}},
		{
			name: "two names", giveAttrNames: []string{attrNameHello, attrNameGeneral},
			wantAttributes: []string{attrNameGeneral, attrNameHello},
		},
		{
			name: "a name and one that does not exist", giveAttrNames: []string{"Foo", attrNameHello},
			wantAttributes: []string{attrNameHello},
		},
		{name: "a prefix", giveAttrNames: []string{"Hel.*"}, wantAttributes: []string{attrNameHello, attrNameHelpMe}},
		{
			name: "two prefixes", giveAttrNames: []string{"Hello.*", "Gen.*"},
			wantAttributes: []string{attrNameGeneral, attrNameHello},
		},
		{name: "system All", giveSystem: []string{attrAll}, wantSystem: []string{
			"ApproximateFirstReceiveTimestamp", "ApproximateReceiveCount", sysSenderID, "SentTimestamp",
		}},
		{name: "one system name", giveSystem: []string{sysSenderID}, wantSystem: []string{sysSenderID}},
		{
			name: "a system name that is not there", giveSystem: []string{sysSenderID, "SequenceNumber"},
			wantSystem: []string{sysSenderID},
		},
	}

	for _, protocol := range protocols {
		for _, tt := range tests {
			t.Run(protocol+"/"+tt.name, func(t *testing.T) {
				c := newStack(t).as(protocol)
				url := c.createQueue("q", map[string]string{attrVisibilityTimeout: "0"})

				sent := c.send(url, "msg", map[string]any{paramMessageAttributes: attrs{
					attrNameHelpMe:  {DataType: typeString, StringValue: "Me"},
					"Hello":         {DataType: typeString, StringValue: "There"},
					attrNameGeneral: {DataType: typeString, StringValue: "Kenobi"},
				}})
				assert.Equal(t, md5Filters, sent.str("MD5OfMessageAttributes"))

				params := map[string]any{}
				if tt.giveAttrNames != nil {
					params[paramMessageAttrNames] = tt.giveAttrNames
				}

				if tt.giveSystem != nil {
					params[paramSystemAttrNames] = tt.giveSystem
				}

				got := c.receive(url, params)
				require.Len(t, got, 1)

				assert.ElementsMatch(t, tt.wantAttributes, keysOf(got[0].userAttrs))
				assert.ElementsMatch(t, tt.wantSystem, keysOf(got[0].system))

				if len(tt.wantAttributes) == 0 {
					assert.Empty(t, got[0].attrsMD5, "the checksum goes with the attributes")
				}
			})
		}
	}
}

func keysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}

	return keys
}

func Test_ReceiveMessage_AttributeNames_stands_in_for_system_names(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			url := c.createQueue("q", nil)
			c.send(url, "msg", nil)

			got := c.receive(url, map[string]any{paramAttributeNames: []string{sysSenderID}})
			require.Len(t, got, 1)
			assert.Equal(t, map[string]string{sysSenderID: defaultAccount}, got[0].system)
		})
	}
}

func Test_ReceiveMessage_MaxNumberOfMessages(t *testing.T) {
	tests := []struct {
		name     string
		give     int
		omit     bool
		wantErr  bool
		wantSize int
	}{
		{name: "the default is one", omit: true, wantSize: 1},
		{name: "zero is out of range", give: 0, wantErr: true},
		{name: "three", give: 3, wantSize: 3},
		{name: "ten", give: 10, wantSize: 10},
		{name: "eleven", give: 11, wantErr: true},
		{name: "negative", give: -2, wantErr: true},
		{name: "minus one takes all", give: -1, wantSize: 12},
	}

	for _, protocol := range protocols {
		for _, tt := range tests {
			t.Run(protocol+"/"+tt.name, func(t *testing.T) {
				c := newStack(t).as(protocol)
				url := c.createQueue("q", nil)

				for i := range 12 {
					c.send(url, "m"+strconv.Itoa(i), nil)
				}

				params := map[string]any{paramQueueURL: url}
				if !tt.omit {
					params[paramMaxMessages] = tt.give
				}

				res := c.call(opReceiveMessage, params)

				if tt.wantErr {
					assert.Equal(t, http.StatusBadRequest, res.status)
					assert.Equal(t, errorCodes(protocol, codeInvalidParameterException, codeInvalidParameter), res.errorCode())
					assert.Equal(t, "Value "+strconv.Itoa(tt.give)+
						" for parameter MaxNumberOfMessages is invalid. Reason: Must be between 1 and 10, if provided.",
						res.errorMessage())

					return
				}

				require.True(t, res.ok())
				assert.Len(t, res.messages(), tt.wantSize)
			})
		}
	}
}

func Test_ReceiveMessage_delivers_the_oldest_first(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			url := c.createQueue("q", nil)

			for i := range 5 {
				c.send(url, "m"+strconv.Itoa(i), nil)
			}

			var bodies []string
			for _, m := range c.receive(url, map[string]any{paramMaxMessages: 5}) {
				bodies = append(bodies, m.body)
			}

			assert.Equal(t, []string{"m0", "m1", "m2", "m3", "m4"}, bodies)
		})
	}
}

func Test_ReceiveMessage_WaitTimeSeconds_is_validated(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			c := newStack(t).as(protocol)
			url := c.createQueue("q", nil)

			res := c.call(opReceiveMessage, map[string]any{paramQueueURL: url, paramWaitTime: 21})

			assert.Equal(t, http.StatusBadRequest, res.status)
			assert.Equal(t, "Value 21 for parameter WaitTimeSeconds is invalid. Reason: Must be >= 0 and <= 20, if provided.",
				res.errorMessage())
		})
	}
}

func Test_SendMessage_is_validated(t *testing.T) {
	tests := []struct {
		name        string
		giveBody    string
		giveExtra   map[string]any
		wantMessage string
		wantJSON    string
		wantQuery   string
	}{
		{
			name: "an empty body", giveBody: "",
			wantJSON: "MissingRequiredParameterException", wantQuery: "MissingParameter",
			wantMessage: "The request must contain the parameter MessageBody.",
		},
		{
			name: "a character XML does not allow", giveBody: "bad\x01",
			wantJSON: "InvalidMessageContents", wantQuery: "InvalidMessageContents",
			wantMessage: "Invalid characters found. Valid unicode characters are " +
				"#x9 | #xA | #xD | #x20 to #xD7FF | #xE000 to #xFFFD | #x10000 to #x10FFFF",
		},
		{
			name: "a body over the maximum size", giveBody: strings.Repeat("a", 1025),
			wantJSON: codeInvalidParameterException, wantQuery: codeInvalidParameter,
			wantMessage: "One or more parameters are invalid. Reason: Message must be shorter than 1024 bytes.",
		},
		{
			name: "attributes count for the size", giveBody: strings.Repeat("a", 1020),
			giveExtra: map[string]any{paramMessageAttributes: attrs{"k": {DataType: typeString, StringValue: "x"}}},
			wantJSON:  codeInvalidParameterException, wantQuery: codeInvalidParameter,
			wantMessage: "One or more parameters are invalid. Reason: Message must be shorter than 1024 bytes.",
		},
		{
			name: "a reserved attribute name", giveBody: "b",
			giveExtra: map[string]any{paramMessageAttributes: attrs{"AWS.x": {DataType: typeString, StringValue: "x"}}},
			wantJSON:  codeInvalidParameterException, wantQuery: codeInvalidParameter,
			wantMessage: "You can't use message attribute names beginning with 'AWS.' or 'Amazon.'. " +
				"These strings are reserved for internal use. Additionally, they cannot start or end with '.'.",
		},
		{
			name: "an attribute name with a bad character", giveBody: "b",
			giveExtra: map[string]any{paramMessageAttributes: attrs{nameWithSpace: {DataType: typeString, StringValue: "x"}}},
			wantJSON:  codeInvalidParameterException, wantQuery: codeInvalidParameter,
			wantMessage: "Message (user) attributes name can only contain upper and lower score characters, " +
				"digits, periods, hyphens and underscores. ",
		},
		{
			name: "an attribute type that is not known", giveBody: "b",
			giveExtra: map[string]any{paramMessageAttributes: attrs{"k": {DataType: "Text", StringValue: "x"}}},
			wantJSON:  codeInvalidParameterException, wantQuery: codeInvalidParameter,
			wantMessage: `Type for parameter MessageAttributes.Attribute_name.DataType must be prefixed` +
				`with "String", "Binary", or "Number", but was: Text`,
		},
		{
			name: "an empty string attribute", giveBody: "b",
			giveExtra: map[string]any{paramMessageAttributes: attrs{"k": {DataType: typeString}}},
			wantJSON:  codeInvalidParameterException, wantQuery: codeInvalidParameter,
			wantMessage: "Message (user) attribute 'k' must contain a non-empty value of type 'String'.",
		},
		{
			name: "a group id with a space", giveBody: "b",
			giveExtra: map[string]any{"MessageGroupId": "group 123"},
			wantJSON:  codeInvalidParameterException, wantQuery: codeInvalidParameter,
			wantMessage: "Value group 123 for parameter MessageGroupId is invalid. Reason: MessageGroupId can only " +
				"include alphanumeric and punctuation characters. 1 to 128 in length.",
		},
		{
			name: "a deduplication id on a standard queue", giveBody: "b",
			giveExtra: map[string]any{"MessageDeduplicationId": "dup"},
			wantJSON:  codeInvalidParameterException, wantQuery: codeInvalidParameter,
			wantMessage: "Value dup for parameter MessageDeduplicationId is invalid. Reason: The request includes " +
				"a parameter that is not valid for this queue type.",
		},
	}

	for _, protocol := range protocols {
		for _, tt := range tests {
			t.Run(protocol+"/"+tt.name, func(t *testing.T) {
				c := newStack(t).as(protocol)
				url := c.createQueue("q", map[string]string{attrMaximumMessageSize: "1024"})

				params := map[string]any{paramQueueURL: url, paramMessageBody: tt.giveBody}
				maps.Copy(params, tt.giveExtra)

				res := c.call(opSendMessage, params)

				assert.Equal(t, http.StatusBadRequest, res.status)
				assert.Equal(t, errorCodes(protocol, tt.wantJSON, tt.wantQuery), res.errorCode())
				assert.Equal(t, tt.wantMessage, res.errorMessage())
			})
		}
	}
}

func Test_Approximate_counts(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newStack(t).as(protocol)
				url := c.createQueue("q", map[string]string{attrVisibilityTimeout: "10"})

				counts := func() [3]string {
					res := c.call(opGetQueueAttributes, map[string]any{paramQueueURL: url, paramAttributeNames: []string{attrAll}})
					attributes := res.attributes()

					return [3]string{
						attributes["ApproximateNumberOfMessages"],
						attributes["ApproximateNumberOfMessagesNotVisible"],
						attributes["ApproximateNumberOfMessagesDelayed"],
					}
				}

				assert.Equal(t, [3]string{"0", "0", "0"}, counts())

				c.send(url, "a", nil)
				c.send(url, "b", nil)
				c.send(url, "c", map[string]any{paramDelaySeconds: 5})
				assert.Equal(t, [3]string{"2", "0", "1"}, counts())

				got := c.receive(url, nil)
				require.Len(t, got, 1)
				assert.Equal(t, [3]string{"1", "1", "1"}, counts())

				time.Sleep(5 * time.Second)
				assert.Equal(t, [3]string{"2", "1", "0"}, counts())

				time.Sleep(5 * time.Second)
				assert.Equal(t, [3]string{"3", "0", "0"}, counts())

				purged := c.call("PurgeQueue", map[string]any{paramQueueURL: url})
				require.True(t, purged.ok())
				assert.Equal(t, [3]string{"0", "0", "0"}, counts())
			})
		})
	}
}
