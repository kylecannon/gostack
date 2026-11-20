package filter_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kylecannon/gostack/internal/service/sns/filter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// awsSnapshots is testdata/aws-snapshots.json: every policy and message in
// LocalStack's tests/aws/services/sns/test_sns_filter_policy.py, all of them
// aws.validated, with the answer AWS recorded in its snapshot.
type awsSnapshots struct {
	Validate   []awsValidateRow   `json:"validate"`
	Attributes []awsAttributesRow `json:"attributes"`
	Body       []awsBodyRow       `json:"body"`
}

type awsValidateRow struct {
	Name      string       `json:"name"`
	Policy    string       `json:"policy"`
	Scope     filter.Scope `json:"scope"`
	Subscribe bool         `json:"subscribe"`
	Error     string       `json:"error"` // empty: accepted
}

type awsAttributesRow struct {
	Name       string                      `json:"name"`
	Policy     string                      `json:"policy"`
	Attributes map[string]filter.Attribute `json:"attributes"`
	Match      bool                        `json:"match"`
}

type awsBodyRow struct {
	Name     string `json:"name"`
	Policy   string `json:"policy"`
	Body     string `json:"body"`
	BodyFile string `json:"bodyFile"`
	Match    bool   `json:"match"`
}

func loadAWS(t *testing.T) awsSnapshots {
	t.Helper()
	data, err := os.ReadFile("testdata/aws-snapshots.json")
	require.NoError(t, err)
	var s awsSnapshots
	require.NoError(t, json.Unmarshal(data, &s))
	return s
}

// Test_Parse_AWSSnapshots checks every refusal against the full message AWS
// sent, with the prefix the calling operation puts before Reason.
func Test_Parse_AWSSnapshots(t *testing.T) {
	for _, tt := range loadAWS(t).Validate {
		t.Run(tt.Name, func(t *testing.T) {
			p, err := filter.Parse(tt.Policy, tt.Scope)
			if tt.Error == "" {
				require.NoError(t, err)
				assert.False(t, p.Empty())
				return
			}
			var ferr *filter.Error
			require.ErrorAs(t, err, &ferr)
			prefix := "Invalid parameter: "
			if tt.Subscribe {
				prefix += "Attributes Reason: "
			}
			assert.Equal(t, tt.Error, prefix+ferr.Reason)
		})
	}
}

// Test_Parse_SnapshotsElsewhere covers the filter-policy rows of
// test_sns.py, whose snapshots give the message for text that is not JSON.
func Test_Parse_SnapshotsElsewhere(t *testing.T) {
	tests := []struct {
		name    string
		give    string
		wantErr string
	}{
		{"set-invalid-json", "{invalidjson}", "FilterPolicy: failed to parse JSON."},
		{"subscribe-invalid-json", "invalid-json", "FilterPolicy: failed to parse JSON."},
		{"empty-removes-policy", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := filter.Parse(tt.give, filter.ScopeMessageAttributes)
			checkErr := filter.CheckJSON(tt.give)
			if tt.wantErr == "" {
				require.NoError(t, err)
				require.NoError(t, checkErr)
				assert.True(t, p.Empty())
				return
			}
			var ferr *filter.Error
			require.ErrorAs(t, err, &ferr)
			assert.Equal(t, tt.wantErr, ferr.Reason)
			require.ErrorAs(t, checkErr, &ferr)
			assert.Equal(t, tt.wantErr, ferr.Reason)
		})
	}
}

func Test_Policy_MatchAttributes_AWSSnapshots(t *testing.T) {
	for _, tt := range loadAWS(t).Attributes {
		t.Run(tt.Name, func(t *testing.T) {
			p, err := filter.Parse(tt.Policy, filter.ScopeMessageAttributes)
			require.NoError(t, err)
			assert.Equal(t, tt.Match, p.MatchAttributes(tt.Attributes))
		})
	}
}

func Test_Policy_MatchBody_AWSSnapshots(t *testing.T) {
	for _, tt := range loadAWS(t).Body {
		t.Run(tt.Name, func(t *testing.T) {
			body := tt.Body
			if tt.BodyFile != "" {
				data, err := os.ReadFile(filepath.Join("testdata", tt.BodyFile))
				require.NoError(t, err)
				body = string(data)
			}
			p, err := filter.Parse(tt.Policy, filter.ScopeMessageBody)
			require.NoError(t, err)
			assert.Equal(t, tt.Match, p.MatchBody(filter.ParseBody(body)))
		})
	}
}
