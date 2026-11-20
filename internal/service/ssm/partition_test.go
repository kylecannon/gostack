package ssm_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service/ssm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParameterARNFollowsThePartitionOfItsRegion: a parameter's ARN names the
// partition of the region of the call. The gateway rewrites a region outside
// the aws partition to us-east-1 before a service sees it, so the test hands
// the provider's dispatcher a request that already carries the region.
func TestParameterARNFollowsThePartitionOfItsRegion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		region    string
		partition string
	}{
		{"us-east-1", "aws"},
		{"cn-north-1", "aws-cn"},
		{"us-gov-west-1", "aws-us-gov"},
		{"us-iso-east-1", "aws-iso"},
		{"us-isob-east-1", "aws-iso-b"},
		{"eu-isoe-west-1", "aws-iso-e"},
		{"us-isof-east-1", "aws-iso-f"},
		{"eusc-de-east-1", "aws-eusc"},
	}

	for _, test := range tests {
		t.Run(test.region, func(t *testing.T) {
			t.Parallel()

			p := ssm.New()
			serve := p.Registration().Serve

			put := serveIn(t, serve, test.region, "PutParameter", `{"Name":"/a/b","Value":"v","Type":"String"}`)
			require.Equal(t, http.StatusOK, put.Code, put.Body.String())

			get := serveIn(t, serve, test.region, "GetParameter", `{"Name":"/a/b"}`)
			require.Equal(t, http.StatusOK, get.Code, get.Body.String())

			var out map[string]any

			require.NoError(t, json.Unmarshal(get.Body.Bytes(), &out))
			assert.Equal(t, "arn:"+test.partition+":ssm:"+test.region+":000000000000:parameter/a/b", param(out)["ARN"])
		})
	}
}

// serveIn runs one json 1.1 call of operation, as account 000000000000 in
// region, through serve.
func serveIn(
	t *testing.T, serve func(http.ResponseWriter, *request.Context), region, operation, body string,
) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "AmazonSSM."+operation)

	rec := httptest.NewRecorder()
	serve(rec, &request.Context{
		Service:   ssmName,
		Operation: operation,
		Protocol:  protocol.KindJSON,
		AccountID: "000000000000",
		Region:    region,
		RequestID: "00000000-0000-4000-8000-000000000000",
		Request:   req,
	})

	return rec
}
