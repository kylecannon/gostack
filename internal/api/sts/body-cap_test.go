package sts_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/api/sts"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/stretchr/testify/assert"
)

// Test_Serve_FormOverTheCapAnswersMalformedQueryString pins what a query
// service answers to a form one byte over GOSTACK_MAX_REQUEST_BODY: 400
// MalformedQueryString, which docs/compat.md records. It sets the process-wide
// cap, so it is not parallel.
func Test_Serve_FormOverTheCapAnswersMalformedQueryString(t *testing.T) {
	const limit = 1024

	protocol.SetMaxBody(limit)
	t.Cleanup(func() { protocol.SetMaxBody(protocol.DefaultMaxBody) })

	const head = "Action=GetCallerIdentity&Pad="

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/",
		strings.NewReader(head+strings.Repeat("a", limit+1-len(head))))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	sts.Serve(sts.Unimplemented{}, rec, &request.Context{
		Service: "sts", Operation: "GetCallerIdentity", Protocol: protocol.KindQuery, RequestID: "req-1", Request: req,
	})

	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "MalformedQueryString")
}
