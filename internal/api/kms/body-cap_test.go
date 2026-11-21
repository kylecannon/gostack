package kms_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/api/kms"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jsonOfSize is a ListKeys body of exactly n bytes.
func jsonOfSize(n int) string {
	const frame = `{"Marker":""}`

	return `{"Marker":"` + strings.Repeat("a", n-len(frame)) + `"}`
}

// serveBody runs the generated Serve for the KMS operation with a json body
// and gives back the status and the answer.
func serveBody(t *testing.T, operation, body string) (int, string) {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "TrentService."+operation)

	rec := httptest.NewRecorder()
	kms.Serve(kms.Unimplemented{}, rec, &request.Context{
		Service: "kms", Operation: operation, Protocol: protocol.KindJSON, RequestID: "req-1", Request: req,
	})

	return rec.Code, rec.Body.String()
}

// Test_Serve_JSONBodyOverTheCap covers the generated readBody of a json-only
// service: a body one byte over GOSTACK_MAX_REQUEST_BODY answers 413
// RequestEntityTooLarge in json's error shape, and a body of exactly the cap is
// read. It sets the process-wide cap, so it is not parallel.
func Test_Serve_JSONBodyOverTheCap(t *testing.T) {
	const limit = 1024

	protocol.SetMaxBody(limit)
	t.Cleanup(func() { protocol.SetMaxBody(protocol.DefaultMaxBody) })

	code, body := serveBody(t, "ListKeys", jsonOfSize(limit+1))

	assert.Equal(t, http.StatusRequestEntityTooLarge, code, body)
	assert.Contains(t, body, "RequestEntityTooLarge")
	assert.Contains(t, body, "1024 bytes")
	assert.Contains(t, body, "GOSTACK_MAX_REQUEST_BODY")
	assert.NotContains(t, body, "MalformedQueryString")

	code, body = serveBody(t, "ListKeys", jsonOfSize(limit))

	require.Equal(t, http.StatusNotImplemented, code, body, "a body of exactly the cap is read")
}

// Test_Serve_JSONBodyUnderTheCapOverTheDecodeBudget pins that a body within the
// cap that would decode into more than its budget is not answered as a body
// over the cap. 400,000 empty grant tokens are 1.2 MB, well under the default
// 16 MiB cap, and decode into more than twice their length plus 16 MiB. The
// cap does not set the budget, so the answer is awsjson.ErrTooLarge: 400
// SerializationException, and it never names GOSTACK_MAX_REQUEST_BODY. It sets
// the process-wide cap, so it is not parallel.
func Test_Serve_JSONBodyUnderTheCapOverTheDecodeBudget(t *testing.T) {
	payload := `{"KeyId":"k","Plaintext":"","GrantTokens":[` + strings.Repeat(`"",`, 399_999) + `""]}`
	require.Less(t, int64(len(payload)), protocol.DefaultMaxBody, "the body is under the cap")

	tests := []struct {
		name  string
		limit int64
	}{
		{"default cap", protocol.DefaultMaxBody},
		{"no cap", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			protocol.SetMaxBody(tt.limit)
			t.Cleanup(func() { protocol.SetMaxBody(protocol.DefaultMaxBody) })

			code, body := serveBody(t, "Encrypt", payload)

			assert.Equal(t, http.StatusBadRequest, code, body)
			assert.Contains(t, body, "SerializationException")
			assert.Contains(t, body, "more than gostack allows")
			assert.NotContains(t, body, "RequestEntityTooLarge")
			assert.NotContains(t, body, "GOSTACK_MAX_REQUEST_BODY")
		})
	}
}
