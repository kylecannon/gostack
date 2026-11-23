package sqs_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const tagKept = "kept"

// A json null in a list or a map is no entry: the decoder drops it
// (awsjson.nulls), as LocalStack's SQS does for the calls below, which the
// LocalStack image answers with 200 and no entry. The calls the image answers
// 500 for, a TypeError in its handler, keep gostack's 200 (docs/compat.md, "Null
// elements").
func Test_a_null_in_a_json_list_or_map_is_no_entry(t *testing.T) {
	t.Parallel()

	c := newStack(t).as(protoJSON)

	created := c.call("CreateQueue", map[string]any{
		"QueueName": "nulls-create", "Attributes": map[string]any{paramVisibilityTimeout: nil},
	})
	require.True(t, created.ok(), "%s %s", created.errorCode(), created.errorMessage())

	url := c.createQueue("nulls", map[string]string{paramVisibilityTimeout: "45"})

	attrs := func() map[string]any {
		res := c.call("GetQueueAttributes", map[string]any{
			paramQueueURL: url, paramAttributeNames: []any{paramVisibilityTimeout},
		})
		require.True(t, res.ok())

		got, _ := res.json["Attributes"].(map[string]any)

		return got
	}

	set := c.call("SetQueueAttributes", map[string]any{
		paramQueueURL: url, "Attributes": map[string]any{paramVisibilityTimeout: nil},
	})
	require.True(t, set.ok(), "%s %s", set.errorCode(), set.errorMessage())
	assert.Equal(t, "45", attrs()[paramVisibilityTimeout], "a null attribute value is not a change to it")

	tag := c.call("TagQueue", map[string]any{paramQueueURL: url, "Tags": map[string]any{"k": nil, tagKept: "v"}})
	require.True(t, tag.ok(), "%s %s", tag.errorCode(), tag.errorMessage())

	tags := c.call("ListQueueTags", map[string]any{paramQueueURL: url})
	require.True(t, tags.ok())
	assert.Equal(t, map[string]any{tagKept: "v"}, tags.json["Tags"], "a null tag value is no tag")

	untag := c.call("UntagQueue", map[string]any{paramQueueURL: url, "TagKeys": []any{nil}})
	require.True(t, untag.ok(), "%s %s", untag.errorCode(), untag.errorMessage())

	tags = c.call("ListQueueTags", map[string]any{paramQueueURL: url})
	assert.Equal(t, map[string]any{tagKept: "v"}, tags.json["Tags"], "a null tag key removes nothing")

	// The image answers 500 for these two (a TypeError); gostack answers as if the
	// list were empty.
	got := c.call("GetQueueAttributes", map[string]any{paramQueueURL: url, paramAttributeNames: []any{nil}})
	assert.Equal(t, http.StatusOK, got.status, "%s %s", got.errorCode(), got.errorMessage())

	recv := c.call("ReceiveMessage", map[string]any{
		paramQueueURL: url, paramAttributeNames: []any{nil}, "MessageAttributeNames": []any{nil},
	})
	assert.Equal(t, http.StatusOK, recv.status, "%s %s", recv.errorCode(), recv.errorMessage())
}
