package dynamodbstreams_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/config"
	"github.com/kylecannon/gostack/internal/gateway"
	"github.com/kylecannon/gostack/internal/persist"
	"github.com/kylecannon/gostack/internal/service"
	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodbstreams"
	"github.com/stretchr/testify/require"
)

// persistedStack is newStack with both services persisted under dir in
// format, as PERSIST_DYNAMODB=1 wires them, and the state on disk loaded.
func persistedStack(t *testing.T, dir string, formats ...persist.Format) (*stack, *persist.Engine) {
	t.Helper()

	eng := persist.New(persist.Config{
		PersistVarSet: true, BaseDir: dir, Formats: formats, Frequency: persist.Immediate,
		MountDetector: func(string) bool { return false },
	})
	t.Cleanup(func() { _ = eng.Close(context.WithoutCancel(t.Context())) })

	s := &stack{ddb: dynamodb.New(func() dynamodb.Settings { return dynamodb.Settings{} }, eng.MarkDirty), region: usEast1}
	s.provider = dynamodbstreams.New(s.ddb)
	regs := []service.Registration{s.ddb.Registration(), s.provider.Registration()}

	for _, reg := range regs {
		require.NotNil(t, reg.Persist, reg.Metadata.Name)
		eng.Register(reg.Metadata.Name, reg.Persist)
	}

	require.NoError(t, eng.Load(t.Context()))

	gw, err := gateway.New(
		func() gateway.Settings { return gateway.Settings{Listen: []string{"0.0.0.0:4566"}} }, regs, nil, eng,
	)
	require.NoError(t, err)

	s.handler = gw.Handler()

	return s, eng
}

// everyType is an item holding every attribute type, empty strings, binaries,
// lists and maps among them, and numbers DynamoDB stores in canonical form.
const everyType = `"s":{"S":"text"},"es":{"S":""},"n":{"N":"-001.50e2"},"b":{"B":"AAE="},` +
	`"ss":{"SS":["a","b"]},"ns":{"NS":["1","2.50"]},"bs":{"BS":["AQ==","Ag=="]},` +
	`"m":{"M":{"em":{"M":{}},"el":{"L":[]},"es":{"S":""},"eb":{"B":""},"deep":{"M":{"x":{"N":"1E+2"}}}}},` +
	`"l":{"L":[{"NULL":true},{"BOOL":false},{"N":"0.000"},{"L":[]}]},"nul":{"NULL":true},"t":{"BOOL":true}`

const persistTable = `{"TableName":"ptab","KeySchema":[{"AttributeName":"pk","KeyType":"HASH"},` +
	`{"AttributeName":"sk","KeyType":"RANGE"}],"AttributeDefinitions":[{"AttributeName":"pk","AttributeType":"S"},` +
	`{"AttributeName":"sk","AttributeType":"N"},{"AttributeName":"g","AttributeType":"S"},` +
	`{"AttributeName":"gs","AttributeType":"B"},{"AttributeName":"ls","AttributeType":"S"}],` +
	`"LocalSecondaryIndexes":[{"IndexName":"lsi","KeySchema":[{"AttributeName":"pk","KeyType":"HASH"},` +
	`{"AttributeName":"ls","KeyType":"RANGE"}],"Projection":{"ProjectionType":"KEYS_ONLY"}}],` +
	`"GlobalSecondaryIndexes":[{"IndexName":"gsi","KeySchema":[{"AttributeName":"g","KeyType":"HASH"},` +
	`{"AttributeName":"gs","KeyType":"RANGE"}],"Projection":{"ProjectionType":"INCLUDE","NonKeyAttributes":["n"]},` +
	`"ProvisionedThroughput":{"ReadCapacityUnits":3,"WriteCapacityUnits":4}}],` +
	`"ProvisionedThroughput":{"ReadCapacityUnits":5,"WriteCapacityUnits":6},"DeletionProtectionEnabled":true,` +
	`"StreamSpecification":{"StreamEnabled":true,"StreamViewType":"NEW_AND_OLD_IMAGES"},` +
	`"Tags":[{"Key":"team","Value":"core"}]}`

// fillPersisted writes the state the round trip compares: a table with both
// kinds of index, items of every type, settings, a stream with records, and a
// second account and region.
func fillPersisted(t *testing.T, s *stack) {
	t.Helper()

	s.ddbOK(t, "CreateTable", persistTable)

	binaries := []string{"AA==", "Aw==", "AQ=="}

	for i := range 12 {
		s.ddbOK(t, "PutItem", fmt.Sprintf(`{"TableName":"ptab","Item":{"pk":{"S":"k%d"},"sk":{"N":"%d.0"},`+
			`"g":{"S":"same"},"gs":{"B":"%s"},"ls":{"S":"l%d"},%s}}`, i%3, i, binaries[i%3], 11-i, everyType))
	}

	s.ddbOK(t, "UpdateItem", `{"TableName":"ptab","Key":{"pk":{"S":"k0"},"sk":{"N":"0"}},`+
		`"UpdateExpression":"SET n = n + :one","ExpressionAttributeValues":{":one":{"N":"1"}}}`)
	s.ddbOK(t, "DeleteItem", `{"TableName":"ptab","Key":{"pk":{"S":"k1"},"sk":{"N":"1"}}}`)
	s.ddbOK(t, "UpdateTimeToLive", `{"TableName":"ptab","TimeToLiveSpecification":{"Enabled":true,"AttributeName":"exp"}}`)
	s.ddbOK(t, "UpdateContinuousBackups",
		`{"TableName":"ptab","PointInTimeRecoverySpecification":{"PointInTimeRecoveryEnabled":true}}`)

	s.ddbOK(t, "CreateTable", tableBody("other", ""))
	s.ddbOK(t, "PutItem", `{"TableName":"other","Item":{"id":{"S":"only"}}}`)

	s.region = "eu-west-1"
	s.ddbOK(t, "CreateTable", tableBody("west", "KEYS_ONLY"))
	s.ddbOK(t, "PutItem", `{"TableName":"west","Item":{"id":{"S":"w"}}}`)
	s.region = usEast1
}

// observe is everything a client can read of the filled state.
func observe(t *testing.T, s *stack) map[string]string {
	t.Helper()

	got := map[string]string{}
	read := func(name, operation, body string) {
		reply := s.call(t, ddbTarget, operation, body)
		require.Equal(t, http.StatusOK, reply.status, reply.body)
		got[name] = canonical(t, reply.body)
	}

	read("describe", "DescribeTable", `{"TableName":"ptab"}`)
	read("scan", "Scan", `{"TableName":"ptab"}`)
	read("gsi", "Query", `{"TableName":"ptab","IndexName":"gsi","KeyConditionExpression":"g = :g",`+
		`"ExpressionAttributeValues":{":g":{"S":"same"}}}`)
	read("lsi", "Query", `{"TableName":"ptab","IndexName":"lsi","KeyConditionExpression":"pk = :p",`+
		`"ExpressionAttributeValues":{":p":{"S":"k0"}},"ScanIndexForward":false}`)
	read("ttl", "DescribeTimeToLive", `{"TableName":"ptab"}`)
	read("tags", "ListTagsOfResource", `{"ResourceArn":"arn:aws:dynamodb:us-east-1:000000000000:table/ptab"}`)
	read("tables", "ListTables", `{}`)
	read("other", "Scan", `{"TableName":"other"}`)

	backups := s.call(t, ddbTarget, "DescribeContinuousBackups", `{"TableName":"ptab"}`)
	require.Equal(t, http.StatusOK, backups.status, backups.body)
	// LatestRestorableDateTime is the time of the call.
	pitr, _, found := strings.Cut(backups.body, "LatestRestorableDateTime")
	require.True(t, found, backups.body)

	got["pitr"] = pitr

	arn := at[string](t, s.ddbOK(t, "DescribeTable", `{"TableName":"ptab"}`), "Table", "LatestStreamArn")
	described := s.call(t, streamsTarget, "DescribeStream", encode(t, map[string]any{keyStreamArn: arn}))
	got["stream"] = canonical(t, described.body)
	got["streams"] = canonical(t, s.call(t, streamsTarget, "ListStreams", `{}`).body)

	shards := at[[]any](t, s.ok(t, "DescribeStream", map[string]any{keyStreamArn: arn}), "StreamDescription", "Shards")
	it := s.iterator(t, arn, at[string](t, shards[0], keyShardID), "TRIM_HORIZON")
	got["records"] = fmt.Sprint(s.records(t, it, 0).Records)

	s.region = "eu-west-1"
	read("west", "Scan", `{"TableName":"west"}`)
	got["westStreams"] = canonical(t, s.call(t, streamsTarget, "ListStreams", `{}`).body)
	s.region = usEast1

	return got
}

// canonical is a json answer with its object members sorted: a map's members
// go out in Go's random order, so two equal answers differ as text.
func canonical(t *testing.T, body string) string {
	t.Helper()

	var v any
	require.NoError(t, json.Unmarshal([]byte(body), &v), body)

	out, err := json.Marshal(v)
	require.NoError(t, err)

	return string(out)
}

// Test_persist_roundTrip saves the filled state through the persistence
// engine, in each format, and loads it into fresh providers: every answer a
// client can read is the same, indexes rebuilt included, and an iterator
// issued before the restart reads the same records after it.
func Test_persist_roundTrip(t *testing.T) {
	for _, format := range []persist.Format{persist.FormatJSON, persist.FormatBinary} {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()

			before, eng := persistedStack(t, dir, format)
			fillPersisted(t, before)
			want := observe(t, before)

			arn := at[string](t, before.ddbOK(t, "DescribeTable", `{"TableName":"ptab"}`), "Table", "LatestStreamArn")
			shards := at[[]any](t, before.ok(t, "DescribeStream", map[string]any{keyStreamArn: arn}),
				"StreamDescription", "Shards")
			it := before.iterator(t, arn, at[string](t, shards[0], keyShardID), "TRIM_HORIZON")

			require.NoError(t, eng.Flush(t.Context()))

			for _, service := range []string{"dynamodb", "dynamodbstreams"} {
				_, err := os.Stat(filepath.Join(dir, service, map[persist.Format]string{
					persist.FormatJSON: "store.json", persist.FormatBinary: "store.bin",
				}[format]))
				require.NoError(t, err, service)
			}

			after, _ := persistedStack(t, dir, format)
			require.Equal(t, want, observe(t, after))

			page := after.records(t, it, 0)
			require.Len(t, page.Records, 14, "12 puts, one update and one delete")
			require.Equal(t, "MODIFY", page.Records[12].EventName)

			// A write after the restart takes the next sequence number.
			after.ddbOK(t, "PutItem", `{"TableName":"ptab","Item":{"pk":{"S":"new"},"sk":{"N":"1"}}}`)
			next := after.records(t, *page.NextShardIterator, 0)
			require.Len(t, next.Records, 1)
			require.Greater(t, next.Records[0].Dynamodb.SequenceNumber, page.Records[13].Dynamodb.SequenceNumber)
		})
	}
}

// Test_persist_streamsOff loads a DynamoDB snapshot without the streams one,
// as PERSIST_DYNAMODBSTREAMS=0 leaves it: the stream is there with no records,
// and its sequence numbers go on from where they were.
func Test_persist_streamsOff(t *testing.T) {
	dir := t.TempDir()

	before, eng := persistedStack(t, dir)
	fillPersisted(t, before)
	require.NoError(t, eng.Flush(t.Context()))
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "dynamodbstreams")))

	after, _ := persistedStack(t, dir)
	arn := at[string](t, after.ddbOK(t, "DescribeTable", `{"TableName":"ptab"}`), "Table", "LatestStreamArn")
	shards := at[[]any](t, after.ok(t, "DescribeStream", map[string]any{keyStreamArn: arn}),
		"StreamDescription", "Shards")
	shard := at[string](t, shards[0], keyShardID)

	require.Empty(t, after.records(t, after.iterator(t, arn, shard, "TRIM_HORIZON"), 0).Records)

	// The shard's window is empty at its saved next number, so LATEST reads
	// nothing rather than slicing past records it does not hold (SQ-348).
	latest := after.iterator(t, arn, shard, "LATEST")
	require.Empty(t, after.records(t, latest, 0).Records)

	after.ddbOK(t, "PutItem", `{"TableName":"ptab","Item":{"pk":{"S":"new"},"sk":{"N":"1"}}}`)
	require.Len(t, after.records(t, latest, 0).Records, 1, "LATEST from before the write reads it")
	got := after.records(t, after.iterator(t, arn, shard, "TRIM_HORIZON"), 0).Records
	require.Len(t, got, 1)
	require.True(t, strings.HasSuffix(got[0].Dynamodb.SequenceNumber, "15"), got[0].Dynamodb.SequenceNumber)
}

// Test_persist_followsDynamoDB checks DynamoDB Streams declares that it
// follows DynamoDB, which is what makes the engine load it after DynamoDB and
// save it right after DynamoDB.
func Test_persist_followsDynamoDB(t *testing.T) {
	ddb := dynamodb.New(func() dynamodb.Settings { return dynamodb.Settings{} }, nil)
	follower, ok := dynamodbstreams.New(ddb).Registration().Persist.(persist.Follower)
	require.True(t, ok)
	require.Equal(t, ddb.Registration().Metadata.Name, follower.Follows())
}

// Test_persist_dynamodbEnablesStreams checks PERSIST_DYNAMODB=1 alone persists
// both services, as localstack-persist's resolve_apis chain does
// (internal/config/persist.go, dynamodb -> dynamodbstreams -> kinesis).
func Test_persist_dynamodbEnablesStreams(t *testing.T) {
	t.Setenv("PERSIST_DYNAMODB", "1")
	t.Setenv("PERSIST_DEFAULT", "0")
	t.Setenv("PERSIST_BASE_DIR", t.TempDir())

	cfg, err := config.Load()
	require.NoError(t, err)

	eng := persist.New(cfg.PersistConfig("dynamodb", "dynamodbstreams", "sqs"))
	require.True(t, eng.Enabled("dynamodb"))
	require.True(t, eng.Enabled("dynamodbstreams"))
	require.False(t, eng.Enabled("sqs"))
}
