package sns

import (
	"testing"

	api "github.com/kylecannon/gostack/internal/api/sns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_sendSpan_deadLetterIsTheCallBuiltDirectly: the dead-letter call spliced
// from the SendMessage a delivery holds is, byte for byte, the call built from
// the message for the dead-letter queue with no trace header, which is what
// was built when the delivery was queued before SQ-327.
func Test_sendSpan_deadLetterIsTheCallBuiltDirectly(t *testing.T) {
	const (
		queueARN = "arn:aws:sqs:us-east-1:000000000000:q.fifo"
		dlqARN   = "arn:aws:sqs:us-west-2:111111111111:dlq.fifo"
		subARN   = "arn:aws:sns:us-east-1:000000000000:t:00000000-0000-0000-0000-000000000000"
	)

	binary := "Binary"
	str := "String"
	value := "v \"quoted\"\n"

	stringAttr := api.MessageAttributeValue{DataType: &str, StringValue: &value}
	binaryAttr := api.MessageAttributeValue{DataType: &binary, BinaryValue: []byte{0, 1, 2, 0xff}}

	cases := map[string]sendParams{
		"body only":   {body: "lost"},
		"escapes":     {body: "a \"quote\", a \\ slash, a \x01 control, é, \xff invalid, </script>\n"},
		"empty body":  {body: ""},
		"trace":       {body: "traced", trace: "Root=1-5759e988-bd862e3fe1be46a994272793"},
		"fifo ids":    {body: "fifo", groupID: "g\"1", dedupID: "d1"},
		"string attr": {body: "attrs", attrs: map[string]api.MessageAttributeValue{"a": stringAttr}},
		"binary attr": {body: "attrs", attrs: map[string]api.MessageAttributeValue{"b": binaryAttr}},
		"everything": {
			body: "all of it", groupID: "g", dedupID: "d", trace: "Root=1-5759e988-bd862e3fe1be46a994272793",
			attrs: map[string]api.MessageAttributeValue{"a": stringAttr, "b": binaryAttr},
		},
	}

	queue, ok := parseARN(queueARN)
	require.True(t, ok)

	dlq, ok := parseARN(dlqARN)
	require.True(t, ok)

	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			primary, span := sendCall(queue, queueARN, "arn:aws:sns:us-east-1:000000000000:t", &params)
			held := string(primary.Body)

			got := span.deadLetter(primary.Body, dlqARN, subARN)()

			untraced := params
			untraced.trace = ""
			want, _ := sendCall(dlq, dlqARN, subARN, &untraced)

			assert.Equal(t, want, got)
			assert.Equal(t, held, string(primary.Body), "building the dead letter left the delivery's own call as it was")
		})
	}
}

// Test_deadLetterCall_isTheCallBuiltDirectly: an HTTP delivery's dead-letter
// call carries the body sent and nothing else, and is built from it when asked.
func Test_deadLetterCall_isTheCallBuiltDirectly(t *testing.T) {
	const (
		dlqARN = "arn:aws:sqs:us-east-1:000000000000:dlq"
		subARN = "arn:aws:sns:us-east-1:000000000000:t:00000000-0000-0000-0000-000000000000"
	)

	body := []byte(`{"Type":"Notification","Message":"a \"quote\"\n"}`)

	build := deadLetterCall(&subscriber{dlq: dlqARN, arn: subARN}, body)
	require.NotNil(t, build)

	target, ok := parseARN(dlqARN)
	require.True(t, ok)

	want, _ := sendCall(target, dlqARN, subARN, &sendParams{body: string(body)})

	assert.Equal(t, want, build())

	assert.Nil(t, deadLetterCall(&subscriber{arn: subARN}, body), "no dead-letter queue, no call")
	assert.Nil(t, deadLetterCall(&subscriber{dlq: "not an arn", arn: subARN}, body), "an unparseable ARN, no call")
}
