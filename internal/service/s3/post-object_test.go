package s3_test

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPostObjectReportsAFailedConditionAsPythonWritesIt: the AccessDenied
// message of a failed policy condition is Python's str() of the parsed
// condition, so an object in it keeps the policy's document order and a float
// is Python's repr. The conditions are sent as written, not marshalled from a Go
// map, which would sort their names.
func TestPostObjectReportsAFailedConditionAsPythonWritesIt(t *testing.T) {
	t.Parallel()

	s := newServer(t)
	created := s.do(call{method: http.MethodPut, target: presignBucket, region: regionUSEast1})
	require.Equal(t, http.StatusOK, created.status, string(created.body))

	tests := []struct {
		name      string
		condition string
		want      string
	}{
		{
			"an object in document order", `["eq", "$key", {"zeta": 1, "alpha": 2, "mid": 3}]`,
			`["eq", "$key", {"zeta": 1, "alpha": 2, "mid": 3}]`,
		},
		{"a million", `["eq", "$key", 1000000.0]`, `["eq", "$key", 1000000.0]`},
		{"ten to the sixteenth", `["eq", "$key", 1e16]`, `["eq", "$key", 1e+16]`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			document := fmt.Sprintf(`{"expiration": %q, "conditions": [%s]}`,
				time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05Z"), test.condition)
			fields := [][2]string{
				{fieldKey, "kpython"},
				{fieldAccessKeyID, accessKeyTest},
				{"signature", "x"},
				{fieldPolicy, base64.StdEncoding.EncodeToString([]byte(document))},
			}

			// A map's order is random, so one rendering proves nothing.
			for range 50 {
				got := postForm(t, s, localstackHost, presignBucket, fields, []byte("hello post"))
				require.Equal(t, http.StatusForbidden, got.status, string(got.body))

				e := got.err(t)
				assert.Equal(t, codeAccessDenied, e.Code)
				assert.Equal(t, "Invalid according to Policy: Policy Condition failed: "+test.want, e.Message)
			}
		})
	}
}
