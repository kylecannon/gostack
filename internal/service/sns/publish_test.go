package sns_test

import (
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	opPublish      = "Publish"
	opPublishBatch = "PublishBatch"
	paramStructure = "MessageStructure"
	valueInvalid   = "ParameterValueInvalid"
	fifoNoGroup    = "Invalid parameter: The MessageGroupId parameter is required for FIFO topics"
	fifoNoDedup    = "Invalid parameter: The topic should either have ContentBasedDeduplication enabled or " +
		"MessageDeduplicationId provided explicitly"
	noDefault = "Invalid parameter: Message Structure - No default entry in JSON message body"
)

func Test_Provider_Publish_validation(t *testing.T) {
	c := newClient(t)
	topic := c.createTopic(paramName, "t")
	fifo := c.createTopic(paramName, "t.fifo", attrKey1, "FifoTopic", attrValue1, textTrue)

	noType := []string{
		attrEntryName, attrColor, attrEntryString, valueRed,
	}

	tests := []struct {
		name    string
		params  []string
		status  int
		code    string
		message string
	}{
		{
			name: "empty subject", params: []string{paramTopicArn, topic, paramMessage, "m", paramSubject, ""},
			status: http.StatusBadRequest, code: invalidParameter, message: "Invalid parameter: Subject",
		},
		{
			// LocalStack reports the empty message before the attribute.
			name: "empty message before attributes", params: append([]string{paramTopicArn, topic}, noType...),
			status: http.StatusBadRequest, code: invalidParameter, message: "Invalid parameter: Empty message",
		},
		{
			name: "bad phone number", params: []string{"PhoneNumber", "abc", paramMessage, "m"},
			status: http.StatusBadRequest, code: invalidParameter,
			message: "Invalid parameter: PhoneNumber Reason: abc is not valid to publish to",
		},
		{
			name: "attribute without a data type", params: append([]string{paramTopicArn, topic, paramMessage, "m"}, noType...),
			status: http.StatusBadRequest, code: "ValidationError",
			message: "1 validation error detected: Value null at 'messageAttributes.color.member.dataType' " +
				"failed to satisfy constraint: Member must not be null",
		},
		{
			name: "attribute of a bad type",
			params: []string{
				paramTopicArn, topic, paramMessage, "m", attrEntryName, "a",
				"MessageAttributes.entry.1.Value.DataType", "Text", attrEntryString, "v",
			},
			status: http.StatusBadRequest, code: valueInvalid,
			message: "The message attribute 'a' has an invalid message attribute type, the set of supported " +
				"type prefixes is Binary, Number, and String.",
		},
		{
			name:   "message too long",
			params: []string{paramTopicArn, topic, paramMessage, strings.Repeat("x", 262145)},
			status: http.StatusBadRequest, code: invalidParameter, message: "Invalid parameter: Message too long",
		},
		{
			name: "missing topic", params: []string{paramTopicArn, topic + "x", paramMessage, "m"},
			status: http.StatusNotFound, code: codeNotFound, message: msgNoTopic,
		},
		{
			name:   "fifo without a group",
			params: []string{paramTopicArn, fifo, paramMessage, "m", paramDedup, "d"},
			status: http.StatusBadRequest, code: invalidParameter, message: fifoNoGroup,
		},
		{
			name:   "fifo without deduplication",
			params: []string{paramTopicArn, fifo, paramMessage, "m", paramGroup, "g"},
			status: http.StatusBadRequest, code: invalidParameter, message: fifoNoDedup,
		},
		{
			name:   "deduplication id on a standard topic",
			params: []string{paramTopicArn, topic, paramMessage, "m", paramDedup, "d"},
			status: http.StatusBadRequest, code: invalidParameter,
			message: "Invalid parameter: MessageDeduplicationId Reason: The request includes MessageDeduplicationId " +
				"parameter that is not valid for this topic type",
		},
		{
			name:   "json without default",
			params: []string{paramTopicArn, topic, paramStructure, "json", paramMessage, `{"sqs":"s"}`},
			status: http.StatusBadRequest, code: invalidParameter, message: noDefault,
		},
		{
			name:   "json that does not parse",
			params: []string{paramTopicArn, topic, paramStructure, "json", paramMessage, `{"default":`},
			status: http.StatusBadRequest, code: invalidParameter,
			message: "Invalid parameter: Message Structure - JSON message body failed to parse",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c.with(t).fails(tt.status, tt.code, tt.message, opPublish, tt.params...)
		})
	}
}

// entry is the PublishBatch parameters of entry n: an Id, a Message and the
// other member name and value pairs.
func entry(n int, id, message string, members ...string) []string {
	p := "PublishBatchRequestEntries.member." + strconv.Itoa(n) + "."
	params := []string{p + "Id", id, p + paramMessage, message}

	for i := 0; i+1 < len(members); i += 2 {
		params = append(params, p+members[i], members[i+1])
	}

	return params
}

func batch(topic string, entries ...[]string) []string {
	params := []string{paramTopicArn, topic}
	for _, e := range entries {
		params = append(params, e...)
	}

	return params
}

func Test_Provider_PublishBatch_validation(t *testing.T) {
	c := newClient(t)
	topic := c.createTopic(paramName, "t")
	fifo := c.createTopic(paramName, "t.fifo", attrKey1, "FifoTopic", attrValue1, textTrue)

	eleven := make([][]string, 11)
	for i := range eleven {
		eleven[i] = entry(i+1, "Id_"+strconv.Itoa(i), "m")
	}

	half := strings.Repeat("x", 140_000)

	tests := []struct {
		name    string
		params  []string
		status  int
		code    string
		message string
	}{
		{
			name: "too many entries", params: batch(fifo, eleven...), status: http.StatusBadRequest,
			code: "TooManyEntriesInBatchRequest", message: "The batch request contains more entries than permissible.",
		},
		{
			name: "no entries", params: batch(topic), status: http.StatusBadRequest,
			code: "EmptyBatchRequest", message: "The batch request doesn't contain any entries.",
		},
		{
			name:   "missing topic before the ids",
			params: batch(topic+"x", entry(1, "1", "m"), entry(2, "1", "m")),
			status: http.StatusNotFound, code: codeNotFound, message: msgNoTopic,
		},
		{
			name: "ids not distinct", params: batch(fifo, entry(1, "1", "m"), entry(2, "1", "m")),
			status: http.StatusBadRequest, code: "BatchEntryIdsNotDistinct",
			message: "Two or more batch entries in the request have the same Id.",
		},
		{
			name: "id too long", params: batch(topic, entry(1, strings.Repeat("a", 81), "m")),
			status: http.StatusBadRequest, code: "InvalidBatchEntryId",
			message: "The Id of a batch entry in the batch request is too long: " + strings.Repeat("a", 81),
		},
		{
			name: "id with a bad character", params: batch(topic, entry(1, "a!b", "m")),
			status: http.StatusBadRequest, code: "InvalidBatchEntryId",
			message: "The Id of a batch entry in the batch request contains an impermissible character: a!b",
		},
		{
			name: "attribute without a data type in the second entry",
			params: batch(topic, entry(1, "1", "m"),
				entry(2, "2", "m", attrEntryName, "x", attrEntryString, "v")),
			status: http.StatusBadRequest, code: "ValidationError",
			message: "1 validation error detected: Value null at " +
				"'publishBatchRequestEntries.2.member.messageAttributes.x.member.dataType' " +
				"failed to satisfy constraint: Member must not be null",
		},
		{
			name: "fifo without a group", params: batch(fifo, entry(1, "1", "m")),
			status: http.StatusBadRequest, code: invalidParameter, message: fifoNoGroup,
		},
		{
			name: "fifo without deduplication", params: batch(fifo, entry(1, "1", "m", paramGroup, "g")),
			status: http.StatusBadRequest, code: invalidParameter, message: fifoNoDedup,
		},
		{
			// test_publish_batch_exceptions: the entry's structure is checked
			// before the FIFO members.
			name:   "json without default before the fifo check",
			params: batch(fifo, entry(1, "1", `{"sqs":"s"}`, paramStructure, "json")),
			status: http.StatusBadRequest, code: invalidParameter, message: noDefault,
		},
		{
			name: "batch too long", params: batch(topic, entry(1, "1", half), entry(2, "2", half)),
			status: http.StatusBadRequest, code: "BatchRequestTooLong",
			message: "The length of all the messages put together is more than the limit.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c.with(t).fails(tt.status, tt.code, tt.message, opPublishBatch, tt.params...)
		})
	}
}

type batchSuccess struct {
	ID             string `xml:"Id"`
	MessageID      string `xml:"MessageId"`
	SequenceNumber string `xml:"SequenceNumber"`
}

type batchFailed struct{}

type batchReply struct {
	Successful []batchSuccess `xml:"PublishBatchResult>Successful>member"`
	Failed     *batchFailed   `xml:"PublishBatchResult>Failed"`
}

func (s *stack) publishBatch(params ...string) batchReply {
	s.t.Helper()

	r := s.call(opPublishBatch, params...)
	require.Equal(s.t, http.StatusOK, r.status, r.body)

	var out batchReply
	require.NoError(s.t, xml.Unmarshal([]byte(r.body), &out))

	return out
}

func Test_Provider_PublishBatch_standard(t *testing.T) {
	s := newStack(t, nil)
	topic := s.createTopic(paramName, "t")
	q := s.createQueue(account, region, "q", nil)
	s.subscribeQueue(topic, q.arn, attrRaw, textTrue)

	out := s.publishBatch(batch(topic, entry(1, "a", "one"), entry(2, "b", "two"))...)

	require.Len(t, out.Successful, 2)
	assert.Equal(t, "a", out.Successful[0].ID)
	assert.Equal(t, "b", out.Successful[1].ID)
	assert.NotEqual(t, out.Successful[0].MessageID, out.Successful[1].MessageID)
	assert.Empty(t, out.Successful[0].SequenceNumber)
	assert.NotNil(t, out.Failed, "Failed is answered, empty")

	msgs := q.await(2)
	assert.ElementsMatch(t, []string{"one", "two"}, []string{msgs[0].Body, msgs[1].Body})
}

// Test_Provider_PublishBatch_fifo publishes a batch whose third entry repeats
// the first's deduplication id: it answers the first's ids and is not
// delivered, and the others arrive in entry order.
func Test_Provider_PublishBatch_fifo(t *testing.T) {
	s := newStack(t, nil)
	topic := s.createTopic(paramName, "t.fifo", attrKey1, "FifoTopic", attrValue1, textTrue)
	q := s.createQueue(account, region, "q.fifo", map[string]string{attrFifoQueue: textTrue})
	s.subscribeQueue(topic, q.arn, attrRaw, textTrue)

	out := s.publishBatch(batch(topic,
		entry(1, "a", "one", paramGroup, "g", paramDedup, "1"),
		entry(2, "b", "two", paramGroup, "g", paramDedup, "2"),
		entry(3, "c", "again", paramGroup, "g", paramDedup, "1"),
	)...)

	require.Len(t, out.Successful, 3)
	assert.Equal(t, out.Successful[0].MessageID, out.Successful[2].MessageID)
	assert.Equal(t, out.Successful[0].SequenceNumber, out.Successful[2].SequenceNumber)
	assert.NotEmpty(t, out.Successful[1].SequenceNumber)
	assert.NotEqual(t, out.Successful[0].SequenceNumber, out.Successful[1].SequenceNumber)

	msgs := q.await(2)
	assert.Equal(t, "one", msgs[0].Body)
	assert.Equal(t, "two", msgs[1].Body)
	assert.Empty(t, q.receive(0))
}
