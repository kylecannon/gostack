package sqs_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	headerNegativeWait = "X-Localstack-Sqs-Override-Wait-Time-Seconds"
	negativeWaitText   = "exception while calling sqs.ReceiveMessage: 'timeout' must be a non-negative number"
)

// Test_a_negative_override_wait_is_answered_as_LocalStack_answers_it pins the
// bytes of SQ-53 finding 4. They were read from a LocalStack 8b9a79f container
// that was sent X-Localstack-Sqs-Override-Wait-Time-Seconds: -1 and -60.
func Test_a_negative_override_wait_is_answered_as_LocalStack_answers_it(t *testing.T) {
	s := newStack(t)
	queueURL := s.as(protoQuery).createQueue("negative-wait", nil)

	for _, wait := range []string{"-1", "-60"} {
		t.Run("json "+wait, func(t *testing.T) {
			got := s.send(httpCall{
				body: `{"QueueUrl":"` + queueURL + `","VisibilityTimeout":30}`,
				headers: map[string]string{
					headerAuthorization: signed("test", usEast1),
					devHeaderType:       jsonType,
					devHeaderTarget:     "AmazonSQS." + opReceiveMessage,
					headerNegativeWait:  wait,
				},
			})

			assert.Equal(t, http.StatusInternalServerError, got.status, got.body)
			assert.JSONEq(t, `{"__type":"CommonServiceException","message":"`+negativeWaitText+`"}`, got.body)
			assert.Equal(t, []string{"InternalError;Sender"}, headerValues(got.header, "x-amzn-query-error"))
			assert.Equal(t, []string{"CommonServiceException"}, headerValues(got.header, "X-Amzn-Errortype"))
		})

		t.Run("query "+wait, func(t *testing.T) {
			got := s.send(httpCall{
				body: "Action=ReceiveMessage&Version=2012-11-05&VisibilityTimeout=30&QueueUrl=" + url.QueryEscape(queueURL),
				headers: map[string]string{
					headerAuthorization: signed("test", usEast1),
					devHeaderType:       formType,
					headerNegativeWait:  wait,
				},
			})

			require.Equal(t, http.StatusInternalServerError, got.status, got.body)
			assert.Contains(t, got.body, "<Code>InternalError</Code>")
			assert.Contains(t, got.body, "<Message>"+negativeWaitText+"</Message>")
			assert.NotContains(t, got.body, "<Type>", "LocalStack names no fault for it")
		})
	}
}
