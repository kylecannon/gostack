package sts_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/api/sts"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_Serve_QueryFormIsReadLikeWerkzeug covers the generated readForm of sts:
// the URL before the body, no ";" separator, a malformed escape as typed.
func Test_Serve_QueryFormIsReadLikeWerkzeug(t *testing.T) {
	t.Parallel()

	var got *sts.AssumeRoleInput

	h := &provider{assumeRole: func(_ context.Context, in *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error) {
		got = in

		return &sts.AssumeRoleOutput{}, nil
	}}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/?RoleArn=url;arn&RoleSessionName=a%FFb",
		strings.NewReader("RoleArn=body&RoleSessionName=body&Policy=%zz"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	sts.Serve(h, rec, &request.Context{
		Service: "sts", Operation: "AssumeRole", Protocol: protocol.KindQuery, AccountID: "000000000000",
		Region: "us-east-1", RequestID: "req-1", Request: req,
	})

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "url;arn", *got.RoleArn)
	assert.Equal(t, "a%FFb", *got.RoleSessionName)
	assert.Equal(t, "%zz", *got.Policy)
}
