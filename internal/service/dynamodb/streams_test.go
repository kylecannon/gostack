package dynamodb_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/stream"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

const streamTable = `{"TableName":"streamed","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],` +
	`"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],"BillingMode":"PAY_PER_REQUEST",` +
	`"StreamSpecification":{"StreamEnabled":true,"StreamViewType":"%s"}}`

// streamCtx is the context the dynamodbstreams service would call with.
func streamCtx(tb testing.TB) context.Context {
	tb.Helper()

	return request.With(tb.Context(), &request.Context{AccountID: accountID, Region: usEast1})
}

// records reads every record of the table's stream.
func (s *stack) records(tb testing.TB, table string) []stream.Record {
	tb.Helper()

	arn := s.latestStreamARN(tb, table)

	info, ok := s.provider.StreamByARN(streamCtx(tb), arn)
	require.True(tb, ok, arn)

	page, err := info.Shard.Read(0, 1000, 1<<20)
	require.NoError(tb, err)

	return page.Records
}

// latestStreamARN is the table's LatestStreamArn.
func (s *stack) latestStreamARN(tb testing.TB, table string) string {
	tb.Helper()

	out := s.ok(tb, "DescribeTable", `{"TableName":"`+table+`"}`)
	described, ok := out["Table"].(map[string]any)
	require.True(tb, ok)

	arn, ok := described["LatestStreamArn"].(string)
	require.True(tb, ok, described)

	return arn
}

// Test_stream_records: every write path leaves one record per change, with the
// images the view type asks for and AWS's SizeBytes (the validated snapshots'
// 12, 16, 46: keys plus images, by item size); an unchanged put, an update
// that sets the same value and a delete of no item leave none.
func Test_stream_records(t *testing.T) {
	s := newStack(t)
	s.ok(t, "CreateTable", fmt.Sprintf(streamTable, "NEW_AND_OLD_IMAGES"))

	s.ok(t, "PutItem", `{"TableName":"streamed","Item":{"id":{"S":"Fred"}}}`)
	s.ok(t, "PutItem", `{"TableName":"streamed","Item":{"id":{"S":"Fred"}}}`)
	s.ok(t, "TransactWriteItems", `{"TransactItems":[{"Put":{"TableName":"streamed","Item":{"id":{"S":"Fred"}}}},`+
		`{"Put":{"TableName":"streamed","Item":{"id":{"S":"NewKey"}}}}]}`)
	s.ok(t, "UpdateItem", `{"TableName":"streamed","Key":{"id":{"S":"NewKey"}},`+
		`"UpdateExpression":"SET attr1 = :a, attr2 = :b",`+
		`"ExpressionAttributeValues":{":a":{"S":"value1"},":b":{"S":"value2"}}}`)
	s.ok(t, "UpdateItem", `{"TableName":"streamed","Key":{"id":{"S":"NewKey"}},"UpdateExpression":"SET attr1 = :a",`+
		`"ExpressionAttributeValues":{":a":{"S":"value1"}}}`)
	s.ok(t, "DeleteItem", `{"TableName":"streamed","Key":{"id":{"S":"nobody"}}}`)
	s.ok(t, "BatchWriteItem", `{"RequestItems":{"streamed":[{"DeleteRequest":{"Key":{"id":{"S":"NewKey"}}}},`+
		`{"PutRequest":{"Item":{"id":{"S":"Fred"},"name":{"S":"Fred"}}}}]}}`)

	got := s.records(t, "streamed")

	type row struct {
		event stream.Event
		size  int64
		seq   int
	}

	rows := make([]row, len(got))
	for i, r := range got {
		rows[i] = row{r.Event, r.SizeBytes, r.Seq}
	}

	require.Equal(t, []row{
		{stream.Insert, 12, 1},
		{stream.Insert, 16, 2},
		{stream.Modify, 46, 3},
		{stream.Remove, 38, 4},
		{stream.Modify, 26, 5},
	}, rows)
	require.Empty(t, got[0].Old)
	require.Empty(t, got[3].New)
	require.Equal(t, "Fred", *value.Unpack(got[4].Old)["id"].S)
}

// Test_stream_viewTypes: KEYS_ONLY keeps neither image, NEW_IMAGE the new one,
// OLD_IMAGE the old one.
func Test_stream_viewTypes(t *testing.T) {
	for _, tc := range []struct {
		view     string
		new, old bool
		size     int64
	}{
		{"KEYS_ONLY", false, false, 6}, {"NEW_IMAGE", true, false, 15}, {"OLD_IMAGE", false, true, 12},
	} {
		t.Run(tc.view, func(t *testing.T) {
			s := newStack(t)
			s.ok(t, "CreateTable", fmt.Sprintf(streamTable, tc.view))
			s.ok(t, "PutItem", `{"TableName":"streamed","Item":{"id":{"S":"Fred"}}}`)
			s.ok(t, "PutItem", `{"TableName":"streamed","Item":{"id":{"S":"Fred"},"n":{"N":"1"}}}`)

			got := s.records(t, "streamed")
			require.Len(t, got, 2)
			require.Equal(t, tc.new, got[1].New != "")
			require.Equal(t, tc.old, got[1].Old != "")
			require.Equal(t, tc.size, got[1].SizeBytes)
			require.Equal(t, "Fred", *got[1].Keys["id"].S)
		})
	}
}

// Test_stream_specification: the image's messages for a StreamSpecification
// it refuses, and a disabled stream keeping LatestStreamArn while it stops
// taking records.
func Test_stream_specification(t *testing.T) {
	s := newStack(t)

	for body, msg := range map[string]string{
		`{"StreamViewType":"NEW_IMAGE"}`:                       "Stream StreamEnabled was null",
		`{"StreamEnabled":true}`:                               "Invalid StreamViewType: Stream ViewType must not be null",
		`{"StreamEnabled":false,"StreamViewType":"NEW_IMAGE"}`: "Stream StreamEnabled was false but a StreamViewType",
	} {
		got := s.call(t, usEast1, "CreateTable", `{"TableName":"bad","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],`+
			`"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],"BillingMode":"PAY_PER_REQUEST",`+
			`"StreamSpecification":`+body+`}`)
		require.Equal(t, http.StatusBadRequest, got.status)
		require.Contains(t, got.body, msg)
	}

	s.ok(t, "CreateTable", fmt.Sprintf(streamTable, "NEW_IMAGE"))
	got := s.call(t, usEast1, "UpdateTable",
		`{"TableName":"streamed","StreamSpecification":{"StreamEnabled":true,"StreamViewType":"KEYS_ONLY"}}`)
	require.Contains(t, got.body, "Table already has an enabled stream")

	s.ok(t, "PutItem", `{"TableName":"streamed","Item":{"id":{"S":"a"}}}`)
	s.ok(t, "UpdateTable", `{"TableName":"streamed","StreamSpecification":{"StreamEnabled":false}}`)
	s.ok(t, "PutItem", `{"TableName":"streamed","Item":{"id":{"S":"b"}}}`)

	got = s.call(t, usEast1, "UpdateTable", `{"TableName":"streamed","StreamSpecification":{"StreamEnabled":false}}`)
	require.Contains(t, got.body, "Table has no stream to disable")

	arn := s.latestStreamARN(t, "streamed")
	require.NotContains(t, s.ok(t, "DescribeTable", `{"TableName":"streamed"}`)["Table"], "StreamSpecification")
	require.Len(t, s.records(t, "streamed"), 1)

	info, ok := s.provider.StreamByARN(streamCtx(t), arn)
	require.True(t, ok)
	require.Equal(t, "DISABLED", info.Status)
	require.Len(t, s.provider.StreamList(streamCtx(t)), 1)

	s.ok(t, "DeleteTable", `{"TableName":"streamed"}`)
	require.Empty(t, s.provider.StreamList(streamCtx(t)))
}

// Test_stream_loadsMainSnapshot restores a streams snapshot that main wrote,
// before a record held its images packed (testdata/snapshot-main-streams.*:
// four tables of every view type, every kind of attribute, an update that
// changes an image and a time-to-live removal), reads its records through the
// stream, and saves the streams snapshot again. The file is main's byte for
// byte, so what changed in memory changed nothing on disk.
func Test_stream_loadsMainSnapshot(t *testing.T) {
	tables, err := os.ReadFile("testdata/snapshot-main-streams.tables.json")
	require.NoError(t, err)

	streams, err := os.ReadFile("testdata/snapshot-main-streams.streams.json")
	require.NoError(t, err)

	// The file's records were written at 2026-11-23T09:54Z, and a record past
	// the 24-hour retention reads as trimmed, so the clock stays a minute past
	// them however long after that the test runs.
	written := time.Date(2026, time.November, 23, 9, 55, 0, 0, time.UTC)

	s := newStack(t)
	dynamodb.SetClock(s.provider, func() time.Time { return written })
	require.NoError(t, s.provider.Restore(bytes.NewReader(tables)))
	require.NoError(t, s.provider.RestoreStreams(bytes.NewReader(streams)))

	orders := s.records(t, "orders")
	require.Len(t, orders, 5, "every record the file held for orders is back")
	require.Equal(t, stream.Modify, orders[2].Event)
	require.Equal(t, "renamed", *value.Unpack(orders[2].New)["name"].S)
	require.Equal(t, "first", *value.Unpack(orders[2].Old)["name"].S)
	require.Empty(t, s.records(t, "keys")[0].New, "a KEYS_ONLY record holds no image")

	last := s.records(t, "ttl")
	require.True(t, last[len(last)-1].Expired, "the time-to-live removal is still marked")

	savedOf(t, s.provider)

	var again bytes.Buffer
	require.NoError(t, s.provider.SnapshotStreams(&again))
	require.Equal(t, string(streams), again.String())
}
