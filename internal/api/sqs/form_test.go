package sqs_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveAt is call with a target, so a test can put parameters in the URL.
func serveAt(t *testing.T, h sqs.Handler, target, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	sqs.Serve(h, rec, &request.Context{
		Service: "sqs", Operation: "SendMessage", Protocol: protocol.KindQuery, AccountID: "000000000000",
		Region: "us-east-1", RequestID: requestID, Request: req,
	})

	return rec
}

// sentBody returns the MessageBody a SendMessage call reached the handler with.
func sentBody(t *testing.T, target, body string) (string, *httptest.ResponseRecorder) {
	t.Helper()

	var got *string

	h := &provider{sendMessage: func(_ context.Context, in *sqs.SendMessageInput) (*sqs.SendMessageOutput, error) {
		got = in.MessageBody

		return &sqs.SendMessageOutput{MessageId: new("m-1")}, nil
	}}

	rec := serveAt(t, h, target, body)
	if got == nil {
		return "", rec
	}

	return *got, rec
}

// Test_Serve_QueryFormIsReadLikeWerkzeug covers the generated readForm.
func Test_Serve_QueryFormIsReadLikeWerkzeug(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		target string
		body   string
		want   string
	}{
		{"a URL argument wins over the body", "/?MessageBody=url", "MessageBody=body", "url"},
		{"a semicolon is not a separator", "/", "MessageBody=a;b&Extra=1", "a;b"},
		{"a semicolon in the URL is not a separator", "/?MessageBody=a;b", "", "a;b"},
		{"a bad escape stays as typed", "/", "MessageBody=a%zz%", "a%zz%"},
		{"an escape that is not UTF-8 stays as typed", "/", "MessageBody=a%ffb", "a%FFb"},
		{"in the URL as well", "/?MessageBody=a%FFb", "", "a%FFb"},
		{"a valid multi-byte escape is decoded", "/", "MessageBody=%E2%82%AC", "€"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, rec := sentBody(t, tt.target, tt.body)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, tt.want, got)
		})
	}
}

func Test_Serve_QueryFormWithRawInvalidUTF8(t *testing.T) {
	t.Parallel()

	for name, in := range map[string][2]string{
		"body":  {"/", "MessageBody=a\xffb"},
		"query": {"/?MessageBody=a\xffb", ""},
	} {
		got, rec := sentBody(t, in[0], in[1])

		assert.Empty(t, got, name)
		assert.Equal(t, http.StatusBadRequest, rec.Code, name)
		assert.Contains(t, rec.Body.String(), "<Code>MalformedQueryString</Code>", name)
	}
}

func Test_Serve_JSONBodyThatIsNotUTF8(t *testing.T) {
	t.Parallel()

	called := false
	h := &provider{sendMessage: func(context.Context, *sqs.SendMessageInput) (*sqs.SendMessageOutput, error) {
		called = true

		return &sqs.SendMessageOutput{}, nil
	}}

	rec := call(t, sqs.Serve, h, "SendMessage", protocol.KindJSON, `{"QueueUrl":"q","MessageBody":"a`+"\xff"+`b"}`)

	assert.False(t, called)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, `{"__type":"SerializationException","message":`+
		`"Start of structure or map found where not expected."}`,
		rec.Body.String())
	assert.Equal(t, []string{"application/x-amz-json-1.0"}, header(rec, "Content-Type"))
}
