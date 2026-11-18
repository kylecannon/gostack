package sqs_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	sqsapi "github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	queueBase = "/000000000000/"
	// invalidBody is a message body that is not UTF-8.
	invalidBody = "a\xffb"
	// escapedBody is what LocalStack stores for "a%FFb", which werkzeug leaves as typed.
	escapedBody = "a%FFb"
)

// formCall sends a signed form to target.
func (s *stack) formCall(target, body string) reply {
	s.t.Helper()

	return s.send(httpCall{
		target:  target,
		body:    body,
		headers: map[string]string{headerAuthorization: signed("test", usEast1), devHeaderType: formType},
	})
}

// jsonCall sends a signed json call to target.
func (s *stack) jsonCall(target, operation, body string) reply {
	s.t.Helper()

	return s.send(httpCall{
		target: target,
		body:   body,
		headers: map[string]string{
			headerAuthorization: signed("test", usEast1),
			devHeaderType:       jsonType,
			"X-Amz-Target":      "AmazonSQS." + operation,
		},
	})
}

// headerValues finds a header by name whatever its case. The generated code
// writes x-amzn-query-error as LocalStack does, in lower case.
func headerValues(h http.Header, name string) []string {
	for key, values := range h {
		if strings.EqualFold(key, name) {
			return values
		}
	}

	return nil
}

// receiveAll reads what the queue holds over both protocols, without hiding
// a message from the next read, and returns the bodies.
func (s *stack) receiveAll(queueURL string) []string {
	s.t.Helper()

	var bodies []string

	for _, proto := range protocols {
		c := s.as(proto)

		res := c.call(opReceiveMessage, map[string]any{
			paramQueueURL: queueURL, "VisibilityTimeout": 0, "MaxNumberOfMessages": 10,
		})

		require.True(s.t, res.ok(), proto)

		for _, m := range res.messages() {
			bodies = append(bodies, m.body)
		}
	}

	return bodies
}

func Test_Form_isReadTheWayLocalStackReadsIt(t *testing.T) {
	s := newStack(t)
	queueURL := s.as(protoQuery).createQueue("form-reading", nil)
	name := "form-reading"

	t.Run("a URL argument wins over the body", func(t *testing.T) {
		got := s.formCall("/?"+paramQueueName+"="+name, "Action=GetQueueUrl&QueueName=other")

		require.Equal(t, http.StatusOK, got.status, got.body)
		assert.Contains(t, got.body, queueURL)
	})

	t.Run("a semicolon is not a separator and a bad escape stays as typed", func(t *testing.T) {
		body := url.Values{"Action": {"SendMessage"}, paramQueueURL: {queueURL}}.Encode() +
			"&MessageBody=a;b%25zz%&Junk=%zz"
		got := s.formCall("/?x=1;y=2", body)

		require.Equal(t, http.StatusOK, got.status, got.body)

		bodies := s.receiveAll(queueURL)
		require.NotEmpty(t, bodies)
		assert.Equal(t, "a;b%zz%", bodies[0])
	})
}

func Test_InvalidUTF8_neverReachesAQueue(t *testing.T) {
	s := newStack(t)
	queueURL := s.as(protoQuery).createQueue("never-invalid", nil)

	form := func(extra string) string {
		return "Action=SendMessage&QueueUrl=" + url.QueryEscape(queueURL) + "&" + extra
	}

	t.Run("a percent escape stays the text it was typed as, in the body", func(t *testing.T) {
		got := s.formCall("/", form("MessageBody=a%FFb"))

		require.Equal(t, http.StatusOK, got.status, got.body)
	})

	t.Run("and in the URL", func(t *testing.T) {
		got := s.formCall("/?MessageBody=a%ffb", form("Version=2012-11-05"))

		require.Equal(t, http.StatusOK, got.status, got.body)
	})

	t.Run("and on a queue URL", func(t *testing.T) {
		got := s.formCall(queueBase+"never-invalid", "Action=SendMessage&MessageBody=a%FFb")

		require.Equal(t, http.StatusOK, got.status, got.body)
	})

	t.Run("a raw byte in a form is refused", func(t *testing.T) {
		for _, target := range []string{"/", queueBase + "never-invalid"} {
			got := s.formCall(target, form("MessageBody="+invalidBody))

			assert.Equal(t, http.StatusBadRequest, got.status, target+" "+got.body)
			assert.Contains(t, got.body, "MalformedQueryString", target)
		}
	})

	t.Run("a raw byte in a json body is LocalStack's 500", func(t *testing.T) {
		body := `{"QueueUrl":"` + queueURL + `","MessageBody":"` + invalidBody + `"}`

		for _, target := range []string{"/", queueBase + "never-invalid"} {
			got := s.jsonCall(target, opSendMessage, body)

			assert.Equal(t, http.StatusInternalServerError, got.status, target+" "+got.body)
			assert.JSONEq(t, `{"__type":"CommonServiceException","message":`+
				`"exception while calling sqs with unknown operation: HTTP body could not be parsed as JSON."}`,
				got.body, target)
			assert.Equal(t, []string{"InternalError;Sender"}, headerValues(got.header, "x-amzn-query-error"), target)
		}
	})

	t.Run("the provider refuses it when a route lets it through", func(t *testing.T) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", http.NoBody)
		rc := &request.Context{
			Service: "sqs", Operation: opSendMessage, Protocol: protocol.KindJSON,
			AccountID: defaultAccount, Region: usEast1, RequestID: "r", Request: req,
		}
		ctx := request.With(context.Background(), rc)

		_, err := s.provider.SendMessage(ctx, &sqsapi.SendMessageInput{
			QueueUrl: new(queueURL), MessageBody: new(invalidBody),
		})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "Invalid characters found")
	})

	t.Run("no response of ReceiveMessage holds invalid UTF-8", func(t *testing.T) {
		for _, proto := range protocols {
			got := s.rawReceive(proto, queueURL)

			assert.True(t, utf8.ValidString(got.body), "%s: %q", proto, got.body)
			assert.Contains(t, got.body, escapedBody, proto)
		}

		for _, b := range s.receiveAll(queueURL) {
			assert.True(t, utf8.ValidString(b))
			assert.True(t, strings.HasPrefix(b, "a%"), b)
		}
	})
}

// rawReceive makes a ReceiveMessage call and returns the response unparsed.
func (s *stack) rawReceive(proto, queueURL string) reply {
	s.t.Helper()

	if proto == protoJSON {
		return s.jsonCall("/", opReceiveMessage,
			`{"QueueUrl":"`+queueURL+`","VisibilityTimeout":0,"MaxNumberOfMessages":10}`)
	}

	return s.formCall("/", "Action=ReceiveMessage&VisibilityTimeout=0&MaxNumberOfMessages=10&QueueUrl="+
		url.QueryEscape(queueURL))
}
