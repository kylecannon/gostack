package dynamodb_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/stretchr/testify/require"
)

const (
	usEast1   = "us-east-1"
	jsonType  = "application/x-amz-json-1.0"
	testHost  = "localhost.localstack.cloud:4566"
	accountID = "000000000000"
)

// stack is the DynamoDB provider behind the real gateway, called in process.
type stack struct {
	handler  http.Handler
	provider *dynamodb.Provider
	settings atomic.Pointer[dynamodb.Settings]
	// changed counts the changes the provider reported itself.
	changed atomic.Int64
}

func newStack(tb testing.TB) *stack {
	tb.Helper()

	s := &stack{}
	s.settings.Store(&dynamodb.Settings{Host: testHost})
	// changed counts DynamoDB's own changes; a stream record reports
	// dynamodbstreams, whose snapshot holds it.
	s.provider = dynamodb.New(func() dynamodb.Settings { return *s.settings.Load() }, func(name string) {
		require.Contains(tb, []string{"dynamodb", "dynamodbstreams"}, name)

		if name == "dynamodb" {
			s.changed.Add(1)
		}
	})

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{"0.0.0.0:4566"}} },
		[]service.Registration{s.provider.Registration()},
		nil,
		nil,
	)
	require.NoError(tb, err)

	s.handler = gw.Handler()

	return s
}

// reply is one response.
type reply struct {
	status int
	body   string
}

// call sends operation with body as the test account in region.
func (s *stack) call(tb testing.TB, region, operation, body string) reply {
	tb.Helper()

	req := httptest.NewRequestWithContext(tb.Context(), http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", jsonType)
	req.Header.Set("X-Amz-Target", "DynamoDB_20120810."+operation)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20240101/"+region+
		"/dynamodb/aws4_request, SignedHeaders=host, Signature=abc")

	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)

	return reply{status: rec.Code, body: rec.Body.String()}
}

// ok sends a call that must succeed and returns its decoded body.
func (s *stack) ok(tb testing.TB, operation, body string) map[string]any {
	tb.Helper()

	got := s.call(tb, usEast1, operation, body)
	require.Equal(tb, http.StatusOK, got.status, got.body)

	var out map[string]any
	require.NoError(tb, json.Unmarshal([]byte(got.body), &out))

	return out
}

// fails sends a call that must fail with code and message.
func (s *stack) fails(tb testing.TB, operation, body, code, message string) {
	tb.Helper()

	got := s.call(tb, usEast1, operation, body)
	require.Equal(tb, http.StatusBadRequest, got.status, got.body)

	var out map[string]any
	require.NoError(tb, json.Unmarshal([]byte(got.body), &out))
	require.Equal(tb, code, out["__type"], got.body)
	require.Equal(tb, message, out["message"], got.body)
}

// path walks decoded JSON by map keys and list indexes.
func path(tb testing.TB, v any, steps ...any) any {
	tb.Helper()

	for _, step := range steps {
		switch key := step.(type) {
		case string:
			m, ok := v.(map[string]any)
			require.True(tb, ok, "not an object at %q: %v", key, v)

			v = m[key]
		case int:
			list, ok := v.([]any)
			require.True(tb, ok, "not a list at %d: %v", key, v)
			require.Less(tb, key, len(list))

			v = list[key]
		default:
			tb.Fatalf("path step %v is neither a key nor an index", step)
		}
	}

	return v
}

// hashTable is a CreateTable body for a provisioned table keyed by id.
func hashTable(name string) string {
	return `{"TableName":"` + name + `","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],` +
		`"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],` +
		`"ProvisionedThroughput":{"ReadCapacityUnits":5,"WriteCapacityUnits":5}}`
}
