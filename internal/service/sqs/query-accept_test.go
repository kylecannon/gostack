package sqs_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	queryVersion  = "Version=2012-11-05"
	quoteBody     = `ba` + "\r" + `r, "quoted", &quot;entity&quot;`
	invalidNameQS = "?Action=GetQueueAttributes&AttributeName.1=Foobar"
	rootURL       = "http://sqs.us-east-1.localhost.localstack.cloud:4566/000000000000/"
)

// getWith makes a GET call to target with the given Accept header.
func (s *stack) getWith(target, accept string) reply {
	s.t.Helper()

	headers := map[string]string{headerAuthorization: signed("test", usEast1)}
	if accept != "" {
		headers[devHeaderAccept] = accept
	}

	return s.send(httpCall{method: http.MethodGet, target: target, headers: headers})
}

// dig follows keys down a decoded JSON document.
func dig(t *testing.T, body string, keys ...string) any {
	t.Helper()

	var current any

	require.NoError(t, json.Unmarshal([]byte(body), &current))

	for _, key := range keys {
		object, isObject := current.(map[string]any)
		require.True(t, isObject, "no object before %s in %s", key, body)

		current = object[key]
	}

	return current
}

func Test_QueryAccept_json(t *testing.T) {
	s := newStack(t)
	queueURL := s.as(protoQuery).createQueue("accept-json", nil)

	t.Run("ListQueues on the root answers in JSON", func(t *testing.T) {
		got := s.getWith("/?Action=ListQueues&"+queryVersion, devTypeAppJS)

		require.Equal(t, http.StatusOK, got.status, got.body)
		require.Equal(t, devTypeAppJS, got.header.Get(devHeaderType))
		require.Equal(t, queueURL, dig(t, got.body, "ListQueuesResponse", "ListQueuesResult", "QueueUrl"))
	})

	t.Run("no Accept header and an XML Accept header answer in XML", func(t *testing.T) {
		for _, accept := range []string{"", devTypeText, "*/*"} {
			got := s.getWith("/?Action=ListQueues&"+queryVersion, accept)

			require.Equal(t, http.StatusOK, got.status, got.body)
			require.Equal(t, devTypeText, got.header.Get(devHeaderType), accept)
			require.Contains(t, got.body, "<ListQueuesResponse ")
		}
	})

	t.Run("a message body keeps its carriage return and quote entities", func(t *testing.T) {
		send := s.as(protoQuery).call("SendMessage", map[string]any{paramQueueURL: queueURL, "MessageBody": quoteBody})
		require.True(t, send.ok())

		got := s.getWith("/?Action=ReceiveMessage&"+queryVersion+"&VisibilityTimeout=0&QueueUrl="+queueURL, devTypeAppJS)

		require.Equal(t, http.StatusOK, got.status, got.body)
		require.Equal(t, quoteBody, dig(t, got.body, "ReceiveMessageResponse", "ReceiveMessageResult", "Message", "Body"))
	})

	t.Run("an error answers in JSON too", func(t *testing.T) {
		got := s.getWith("/000000000000/accept-json"+invalidNameQS, devTypeAppJS)

		require.Equal(t, http.StatusBadRequest, got.status, got.body)
		require.Equal(t, devTypeAppJS, got.header.Get(devHeaderType))
		require.Equal(t, "Sender", dig(t, got.body, "ErrorResponse", "Error", "Type"))
		require.Equal(t, "InvalidAttributeName", dig(t, got.body, "ErrorResponse", "Error", "Code"))
	})
}

func Test_QueryAccept_queueURLErrorsAreSenderFaults(t *testing.T) {
	s := newStack(t)
	s.as(protoQuery).createQueue("sender-fault", nil)

	t.Run("a call to a queue URL says Sender", func(t *testing.T) {
		got := s.getWith("/000000000000/sender-fault"+invalidNameQS, "")

		require.Equal(t, http.StatusBadRequest, got.status, got.body)
		require.Contains(t, got.body, "<Code>InvalidAttributeName</Code>")
		require.Contains(t, got.body, "<Message>Unknown Attribute Foobar.</Message><Type>Sender</Type></Error>")
		require.Equal(t, 1, strings.Count(got.body, "<Type>"))
	})

	t.Run("a call to the root does not, as in LocalStack", func(t *testing.T) {
		got := s.as(protoQuery).call("GetQueueAttributes", map[string]any{
			paramQueueURL:    rootURL + "sender-fault",
			"AttributeNames": []string{"Foobar"},
		})

		require.Equal(t, "InvalidAttributeName", got.errorCode())
		require.Equal(t, http.StatusBadRequest, got.status)
	})
}

func Test_QueryAccept_emptyReceiveIsAShortElement(t *testing.T) {
	s := newStack(t)
	s.as(protoQuery).createQueue("empty-receive", nil)

	got := s.getWith("/000000000000/empty-receive?Action=ReceiveMessage", "")

	require.Equal(t, http.StatusOK, got.status, got.body)
	require.Contains(t, got.body, "<ReceiveMessageResult/>")
}
