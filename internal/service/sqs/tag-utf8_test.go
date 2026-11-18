package sqs_test

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_a_tag_with_an_escape_that_is_not_UTF_8_is_taken is SQ-65. TagQueue and
// CreateQueue never answer InvalidMessageContents on the wire: LocalStack 8b9a79f
// answers 200 for "%FF" in a tag value or key and keeps the text as typed
// (probed in verification/SQ-145/ls-probe.out), and the query reader here does
// the same, so a tag always reaches the provider as valid UTF-8. checkTags stays
// for a caller that bypasses the readers; it is not reachable from the wire.
func Test_a_tag_with_an_escape_that_is_not_UTF_8_is_taken(t *testing.T) {
	t.Parallel()

	c := newStack(t).as(protoQuery)
	queueURL := c.createQueue("q", nil)

	res := c.rawBody("TagQueue", "Action=TagQueue&QueueUrl="+url.QueryEscape(queueURL)+
		"&Tag.1.Key=k%FF&Tag.1.Value=a%FFb&Tag.2.Key=k2&Tag.2.Value=%C3%A9")

	require.True(t, res.ok(), "%s %s", res.errorCode(), res.errorMessage())

	listed := c.call("ListQueueTags", map[string]any{paramQueueURL: queueURL})
	assert.Equal(t, map[string]string{"k%FF": "a%FFb", "k2": "é"}, listed.tags())

	created := c.rawBody("CreateQueue", "Action=CreateQueue&QueueName=q2&Tag.1.Key=k&Tag.1.Value=a%FFb")
	require.True(t, created.ok(), "%s %s", created.errorCode(), created.errorMessage())
}

// Test_a_json_tag_that_is_not_valid_UTF_8_is_refused_by_the_reader: a lone
// surrogate escape is a 400 SerializationException, which LocalStack accepts and
// gostack does not (docs/compat.md).
func Test_a_json_tag_that_is_not_valid_UTF_8_is_refused_by_the_reader(t *testing.T) {
	t.Parallel()

	c := newStack(t).as(protoJSON)
	queueURL := c.createQueue("q", nil)

	res := c.rawBody("TagQueue", `{"QueueUrl":"`+queueURL+`","Tags":{"k":"a\ud83d"}}`)

	assert.Equal(t, 400, res.status)
	assert.Equal(t, "SerializationException", res.errorCode())
}
