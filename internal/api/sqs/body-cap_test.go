package sqs_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/api/sqs"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/stretchr/testify/assert"
)

// Test_Serve_BodyOverTheCapKeepsTheQueryAnswer pins what SQS answers to a body
// one byte over GOSTACK_MAX_REQUEST_BODY in each of its protocols: 400
// MalformedQueryString, json included, which the SQS developer API tests and
// docs/compat.md record. Other json services answer 413 (see kms). It sets the
// process-wide cap, so it is not parallel.
func Test_Serve_BodyOverTheCapKeepsTheQueryAnswer(t *testing.T) {
	const limit = 1024

	protocol.SetMaxBody(limit)
	t.Cleanup(func() { protocol.SetMaxBody(protocol.DefaultMaxBody) })

	tests := []struct {
		name, contentType, target, body string
		kind                            protocol.Kind
	}{
		{
			name: "json", contentType: "application/x-amz-json-1.0", target: "AmazonSQS.ListQueues",
			body: `{"QueueNamePrefix":"` + strings.Repeat("a", limit+1-len(`{"QueueNamePrefix":""}`)) + `"}`,
			kind: protocol.KindJSON,
		},
		{
			name: "query", contentType: "application/x-www-form-urlencoded", target: "",
			body: "Action=ListQueues&QueueNamePrefix=" + strings.Repeat("a", limit+1-len("Action=ListQueues&QueueNamePrefix=")),
			kind: protocol.KindQuery,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", tt.contentType)

			if tt.target != "" {
				req.Header.Set("X-Amz-Target", tt.target)
			}

			rec := httptest.NewRecorder()
			sqs.Serve(sqs.Unimplemented{}, rec, &request.Context{
				Service: "sqs", Operation: "ListQueues", Protocol: tt.kind, RequestID: requestID, Request: req,
			})

			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "MalformedQueryString")
		})
	}
}
