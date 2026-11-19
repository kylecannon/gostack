package sqs_test

import (
	"encoding/json"
	"maps"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rawCall sends one operation with the member written exactly as given: a query
// form value, or a json value. Both are what LocalStack's parser reads.
func (c caller) rawCall(operation, queueURL, member, raw string) result {
	c.s.t.Helper()

	if c.protocol == protoJSON {
		return c.rawBody(operation, `{"QueueUrl":"`+queueURL+`","`+member+`":`+raw+`}`)
	}

	return c.rawBody(operation, "Action="+operation+"&QueueUrl="+url.QueryEscape(queueURL)+"&"+member+"="+raw)
}

// rawBody sends a request body as it is: json for the json protocol, a form for
// the query protocol.
func (c caller) rawBody(operation, body string) result {
	c.s.t.Helper()

	headers := map[string]string{headerAuthorization: signed(c.key, c.region)}

	if c.protocol == protoJSON {
		headers["Content-Type"] = jsonType
		headers["X-Amz-Target"] = "AmazonSQS." + operation
	} else {
		headers["Content-Type"] = formType
	}

	return c.decode(c.s.send(httpCall{host: c.host, body: body, headers: headers, tls: c.tls}))
}

// quoteJSON is s as a json string literal.
func quoteJSON(t *testing.T, s string) string {
	t.Helper()

	encoded, err := json.Marshal(s)
	require.NoError(t, err)

	return string(encoded)
}

// Test_numbers_are_read_the_way_LocalStack_reads_them is SQ-63. The inputs were
// sent to LocalStack 8b9a79f as ReceiveMessage MaxNumberOfMessages
// (verification/SQ-145/ls-probe.out): the accepted ones answered 200, the
// refused ones a 500 that gostack answers as a 400.
func Test_numbers_are_read_the_way_LocalStack_reads_them(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		protocol string
		raw      string
		wantOK   bool
	}{
		{"query padded", protoQuery, "%205", true},
		{"query underscore", protoQuery, "1_0", true},
		{"query plus sign", protoQuery, "%2B5", true},
		{"query Arabic-Indic digit", protoQuery, "%D9%A5", true},
		{"query fraction", protoQuery, "5.0", false},
		{"query hex", protoQuery, "0x10", false},
		{"query empty", protoQuery, "", false},
		{"query exponent", protoQuery, "1e1", false},
		{"json string", protoJSON, `"5"`, true},
		{"json padded string", protoJSON, `" 5"`, true},
		{"json underscore string", protoJSON, `"1_0"`, true},
		{"json whole float", protoJSON, `5.0`, true},
		{"json fraction", protoJSON, `5.7`, true},
		{"json exponent", protoJSON, `1e1`, true},
		{"json true", protoJSON, `true`, true},
		{"json null", protoJSON, `null`, true},
		{"json word", protoJSON, `"abc"`, false},
		{"json string fraction", protoJSON, `"5.0"`, false},
		{"json list", protoJSON, `[5]`, false},
		{"json overflowing float", protoJSON, `1e400`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := newStack(t).as(tt.protocol)
			queueURL := c.createQueue("q", nil)

			res := c.rawCall(opReceiveMessage, queueURL, paramMaxMessages, tt.raw)

			if tt.wantOK {
				require.True(t, res.ok(), "%s %s", res.errorCode(), res.errorMessage())

				return
			}

			assert.Equal(t, 400, res.status)
			assert.Contains(t, []string{"InvalidParameterValue", "SerializationException"}, res.errorCode())
		})
	}
}

// Test_numbers_past_int32_reach_the_range_check: LocalStack takes any size of
// integer and its handler refuses it with the range message, echoing the number
// it was sent. gostack reads an int32, and still echoes the number sent.
func Test_numbers_past_int32_reach_the_range_check(t *testing.T) {
	t.Parallel()

	// A bound sent as itself is echoed as itself. The json-only forms reach the
	// clamp through the lenient reader alone, which keeps the body for the echo.
	sentAs := map[string]string{
		"3000000000":           "3000000000",
		"-3000000000":          "-3000000000",
		"99999999999999999999": "99999999999999999999",
		"2147483647":           "2147483647",
		"-2147483648":          "-2147483648",
	}
	jsonSentAs := map[string]string{
		`"3000000000"`: "3000000000",
		`3e9`:          "3000000000",
		`-3.5e9`:       "-3500000000",
	}

	for _, protocol := range protocols {
		cases := maps.Clone(sentAs)
		if protocol == protoJSON {
			maps.Copy(cases, jsonSentAs)
		}

		for raw, sent := range cases {
			for param, reason := range map[string]string{
				paramMaxMessages: "Must be between 1 and 10, if provided.",
				paramWaitTime:    "Must be >= 0 and <= 20, if provided.",
			} {
				t.Run(protocol+" "+param+" "+raw, func(t *testing.T) {
					t.Parallel()

					c := newStack(t).as(protocol)
					queueURL := c.createQueue("q", nil)

					res := c.rawCall(opReceiveMessage, queueURL, param, raw)

					assert.Equal(t, 400, res.status)
					assert.Contains(t, res.errorCode(), "InvalidParameterValue")
					assert.Equal(t, "Value "+sent+" for parameter "+param+" is invalid. Reason: "+reason,
						res.errorMessage())
				})
			}
		}
	}
}

// Test_blobs_are_read_the_way_LocalStack_reads_them: LocalStack decodes a
// binary attribute with b64decode(), which skips characters outside the alphabet
// and ignores what follows the padding, and refuses unpadded text.
func Test_blobs_are_read_the_way_LocalStack_reads_them(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		value  string
		wantOK bool
	}{
		{"padded", "aGVsbG8=", true},
		{"over-padded", "aGVsbG8==", true},
		{"data after the padding", "aGVsbG8=aGVsbG8=", true},
		{"a skipped character", "aGV$sbG8=", true},
		{"a newline", "aGVs\nbG8=", true},
		{"an empty string", "", true},
		{"unpadded", "aGVsbG8", false},
		{"a dangling data character", "aGVsb", false},
		{"not ASCII", "aGVsäbG8=", false},
	}

	for _, protocol := range protocols {
		for _, tt := range tests {
			t.Run(protocol+" "+tt.name, func(t *testing.T) {
				t.Parallel()

				c := newStack(t).as(protocol)
				queueURL := c.createQueue("q", nil)

				var res result

				if protocol == protoJSON {
					res = c.rawBody("SendMessage", `{"QueueUrl":"`+queueURL+`","MessageBody":"b","MessageAttributes":`+
						`{"a":{"DataType":"Binary","BinaryValue":`+quoteJSON(t, tt.value)+`}}}`)
				} else {
					res = c.rawBody("SendMessage", "Action=SendMessage&QueueUrl="+url.QueryEscape(queueURL)+
						"&MessageBody=b&MessageAttribute.1.Name=a&MessageAttribute.1.Value.DataType=Binary"+
						"&MessageAttribute.1.Value.BinaryValue="+url.QueryEscape(tt.value))
				}

				if tt.wantOK {
					require.True(t, res.ok(), "%s %s", res.errorCode(), res.errorMessage())

					return
				}

				assert.Equal(t, 400, res.status)
			})
		}
	}
}
