package secretsmanager_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service/secretsmanager"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSecretARNFollowsThePartitionOfItsRegion: a secret's ARN names the
// partition of the region of the call. The gateway rewrites a region outside
// the aws partition to us-east-1 before a service sees it, so the test hands
// the provider's dispatcher a request that already carries the region.
func TestSecretARNFollowsThePartitionOfItsRegion(t *testing.T) {
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

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/",
				strings.NewReader(`{"Name":"s","SecretString":"v","ClientRequestToken":"`+token1+`"}`))
			req.Header.Set("Content-Type", "application/x-amz-json-1.1")
			req.Header.Set("X-Amz-Target", "secretsmanager.CreateSecret")

			rec := httptest.NewRecorder()
			secretsmanager.New(nil).Registration().Serve(rec, &request.Context{
				Service:   "secretsmanager",
				Operation: "CreateSecret",
				Protocol:  protocol.KindJSON,
				AccountID: "000000000000",
				Region:    test.region,
				RequestID: "00000000-0000-4000-8000-000000000000",
				Request:   req,
			})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			var out map[string]any

			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
			assert.Regexp(t,
				"^"+regexp.QuoteMeta("arn:"+test.partition+":secretsmanager:"+test.region+":000000000000:secret:s-")+
					"[A-Za-z]{6}$",
				out["ARN"])
		})
	}
}
