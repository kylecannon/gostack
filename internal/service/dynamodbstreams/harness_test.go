package dynamodbstreams_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodbstreams"
	"github.com/stretchr/testify/require"
)

const (
	usEast1   = "us-east-1"
	accountID = "000000000000"
	jsonType  = "application/x-amz-json-1.0"
)

// The X-Amz-Target prefixes of the two services, which both sign as dynamodb.
const (
	ddbTarget     = "DynamoDB_20120810."
	streamsTarget = "DynamoDBStreams_20120810."
)

// stack is DynamoDB and DynamoDB Streams behind the real gateway, called in
// process, as cmd/gostack registers them.
type stack struct {
	handler  http.Handler
	ddb      *dynamodb.Provider
	provider *dynamodbstreams.Provider
	// region is the region calls are signed for, us-east-1 unless set.
	region string
}

func newStack(tb testing.TB) *stack {
	tb.Helper()

	s := &stack{ddb: dynamodb.New(func() dynamodb.Settings { return dynamodb.Settings{} }), region: usEast1}
	s.provider = dynamodbstreams.New(s.ddb)

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{"0.0.0.0:4566"}} },
		[]service.Registration{s.ddb.Registration(), s.provider.Registration()},
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

// call sends target+operation with body, signed as dynamodb in us-east-1.
func (s *stack) call(tb testing.TB, target, operation, body string) reply {
	tb.Helper()

	req := httptest.NewRequestWithContext(tb.Context(), http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", jsonType)
	req.Header.Set("X-Amz-Target", target+operation)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20240101/"+s.region+
		"/dynamodb/aws4_request, SignedHeaders=host, Signature=abc")

	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)

	return reply{status: rec.Code, body: rec.Body.String()}
}

// ddbOK sends a DynamoDB call that must succeed.
func (s *stack) ddbOK(tb testing.TB, operation, body string) map[string]any {
	tb.Helper()

	return decode(tb, s.call(tb, ddbTarget, operation, body))
}

// ok sends a DynamoDB Streams call that must succeed.
func (s *stack) ok(tb testing.TB, operation string, in any) map[string]any {
	tb.Helper()

	return decode(tb, s.call(tb, streamsTarget, operation, encode(tb, in)))
}

// fail sends a DynamoDB Streams call that must fail with code, and returns its
// message.
func (s *stack) fail(tb testing.TB, operation string, in any, code string) string {
	tb.Helper()

	got := s.call(tb, streamsTarget, operation, encode(tb, in))
	require.Equal(tb, http.StatusBadRequest, got.status, got.body)

	var out struct {
		Type    string `json:"__type"`
		Message string `json:"message"`
	}
	require.NoError(tb, json.Unmarshal([]byte(got.body), &out))
	require.Equal(tb, code, out.Type, got.body)

	return out.Message
}

func encode(tb testing.TB, in any) string {
	tb.Helper()

	if s, ok := in.(string); ok {
		return s
	}

	b, err := json.Marshal(in)
	require.NoError(tb, err)

	return string(b)
}

func decode(tb testing.TB, got reply) map[string]any {
	tb.Helper()
	require.Equal(tb, http.StatusOK, got.status, got.body)

	var out map[string]any
	require.NoError(tb, json.Unmarshal([]byte(got.body), &out))

	return out
}

// tableBody is a CreateTable body for table keyed by the string id, with a
// stream of view, or none when view is "".
func tableBody(table, view string) string {
	spec := ""
	if view != "" {
		spec = `,"StreamSpecification":{"StreamEnabled":true,"StreamViewType":"` + view + `"}`
	}

	return `{"TableName":"` + table + `","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],` +
		`"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],"BillingMode":"PAY_PER_REQUEST"` + spec + `}`
}

// createStream creates table with a stream of view and returns the stream's
// ARN and its shard's id.
func (s *stack) createStream(tb testing.TB, table, view string) (arn, shard string) {
	tb.Helper()

	out := s.ddbOK(tb, "CreateTable", tableBody(table, view))
	arn = at[string](tb, out, "TableDescription", "LatestStreamArn")

	described := s.ok(tb, "DescribeStream", map[string]any{keyStreamArn: arn})
	shards := at[[]any](tb, described, "StreamDescription", "Shards")
	require.Len(tb, shards, 1)

	return arn, at[string](tb, shards[0], keyShardID)
}

// iterator returns an iterator of typ on the stream's shard.
func (s *stack) iterator(tb testing.TB, arn, shard, typ string) string {
	tb.Helper()

	out := s.ok(tb, "GetShardIterator", map[string]any{keyStreamArn: arn, keyShardID: shard, keyIteratorType: typ})

	return at[string](tb, out, keyShardIterator)
}

// Request members the tests name often.
const (
	keyStreamArn     = "StreamArn"
	keyShardID       = "ShardId"
	keyShardIterator = "ShardIterator"
	keyLimit         = "Limit"
	keyIteratorType  = "ShardIteratorType"
	opGetRecords     = "GetRecords"
)

// at is the value at path in v, a decoded JSON object, as a T.
func at[T any](tb testing.TB, v any, path ...string) T {
	tb.Helper()

	for _, member := range path {
		object, ok := v.(map[string]any)
		require.True(tb, ok, "%v is not an object at %s", v, member)

		v = object[member]
	}

	out, ok := v.(T)
	require.True(tb, ok, "%v at %v", v, path)

	return out
}

// streamRecord is one record's dynamodb member.
type streamRecord struct {
	Keys           map[string]any `json:"Keys"`
	NewImage       map[string]any `json:"NewImage"`
	OldImage       map[string]any `json:"OldImage"`
	SequenceNumber string         `json:"SequenceNumber"`
	SizeBytes      int64          `json:"SizeBytes"`
	StreamViewType string         `json:"StreamViewType"`
	Approximate    float64        `json:"ApproximateCreationDateTime"`
}

// key is the string id the record is keyed by.
func (r *streamRecord) key(tb testing.TB) string {
	tb.Helper()

	return at[string](tb, r.Keys, "id", "S")
}

// streamsRecord is one GetRecords record.
type streamsRecord struct {
	EventID      string         `json:"eventID"`
	EventName    string         `json:"eventName"`
	EventVersion string         `json:"eventVersion"`
	EventSource  string         `json:"eventSource"`
	AwsRegion    string         `json:"awsRegion"`
	UserIdentity map[string]any `json:"userIdentity"`
	Dynamodb     streamRecord   `json:"dynamodb"`
}

// page is one GetRecords answer.
type page struct {
	Records           []streamsRecord `json:"Records"`
	NextShardIterator *string         `json:"NextShardIterator"`
}

// records reads one page at it, with limit when it is above 0.
func (s *stack) records(tb testing.TB, it string, limit int) page {
	tb.Helper()

	in := map[string]any{keyShardIterator: it}
	if limit > 0 {
		in[keyLimit] = limit
	}

	got := s.call(tb, streamsTarget, opGetRecords, encode(tb, in))
	require.Equal(tb, http.StatusOK, got.status, got.body)

	var out page
	require.NoError(tb, json.Unmarshal([]byte(got.body), &out))
	require.Contains(tb, got.body, `"Records":[`, "Records is always there")

	return out
}

// ctx is the request context the stream interface sees.
func ctx(tb testing.TB) *request.Context {
	tb.Helper()

	return &request.Context{AccountID: accountID, Region: usEast1}
}
