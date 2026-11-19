package dynamodb_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_ServeValidated_SnapshotMessages sends the input of each
// test_dynamodb.py test whose snapshot pins a constraint message, and checks
// the generated checks answer that message. The key names the snapshot entry.
func Test_ServeValidated_SnapshotMessages(t *testing.T) {
	t.Parallel()

	cases := []struct {
		key  string
		op   string
		body string
		want string
	}{
		{
			// An empty list is set, so its length is checked; an absent one
			// is not.
			key:  "test_dynamodb_execute_statement_empy_parameter invalid-param-error",
			op:   "ExecuteStatement",
			body: `{"Statement":"SELECT * FROM t","Parameters":[]}`,
			want: "1 validation error detected: Value '[]' at 'parameters' failed to satisfy constraint: " +
				"Member must have length greater than or equal to 1",
		},
		{
			key: "test_kinesis_streaming_destination_crud update-bad-precision",
			op:  "UpdateKinesisStreamingDestination",
			body: `{"TableName":"t","StreamArn":"arn:aws:kinesis:us-east-1:000000000000:stream/s",` +
				`"UpdateKinesisStreamingConfiguration":{"ApproximateCreationDateTimePrecision":"SECOND"}}`,
			want: "1 validation error detected: Value 'SECOND' at " +
				"'updateKinesisStreamingConfiguration.approximateCreationDateTimePrecision' failed to satisfy constraint: " +
				"Member must satisfy enum value set: [MILLISECOND, MICROSECOND]",
		},
	}

	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			t.Parallel()

			code, body := serveJSON(t, c.op, c.body)
			assert.Equal(t, http.StatusBadRequest, code)
			assert.Equal(t, "ValidationException", body["__type"])
			assert.Equal(t, c.want, body["message"])
		})
	}
}

// Test_ServeValidated_AbsentListIsNotMeasured checks that a list member the
// request leaves out is not held to its min length.
func Test_ServeValidated_AbsentListIsNotMeasured(t *testing.T) {
	t.Parallel()

	_, body := serveJSON(t, "ExecuteStatement", `{"Statement":"SELECT * FROM t"}`)
	assert.NotEqual(t, "ValidationException", body["__type"])
}

func serveJSON(t *testing.T, op, payload string) (int, map[string]any) {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "DynamoDB_20120810."+op)

	rec := httptest.NewRecorder()
	dynamodb.ServeValidated(&dynamodb.Unimplemented{}, rec, &request.Context{
		Service: "dynamodb", Operation: op, Protocol: protocol.KindJSON, RequestID: "req-1", Request: req,
	})

	var body map[string]any

	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())

	return rec.Code, body
}
