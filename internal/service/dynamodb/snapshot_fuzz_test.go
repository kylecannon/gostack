package dynamodb_test

import (
	"bytes"
	"testing"

	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/stretchr/testify/require"
)

// seedSnapshot is a snapshot with a table that has both kinds of index, a
// stream with records, tags and a time to live setting, for the fuzz seeds.
func seedSnapshot(tb testing.TB) (tables, streams []byte) {
	tb.Helper()

	s := newStack(tb)
	s.ok(tb, opCreate, `{"TableName":"seed","KeySchema":[{"AttributeName":"pk","KeyType":"HASH"},`+
		`{"AttributeName":"sk","KeyType":"RANGE"}],"AttributeDefinitions":[{"AttributeName":"pk","AttributeType":"S"},`+
		`{"AttributeName":"sk","AttributeType":"N"},{"AttributeName":"g","AttributeType":"B"}],`+
		`"BillingMode":"PAY_PER_REQUEST","GlobalSecondaryIndexes":[{"IndexName":"gsi1","KeySchema":`+
		`[{"AttributeName":"g","KeyType":"HASH"}],"Projection":{"ProjectionType":"ALL"}}],`+
		`"StreamSpecification":{"StreamEnabled":true,"StreamViewType":"NEW_AND_OLD_IMAGES"},`+
		`"Tags":[{"Key":"k","Value":"v"}]}`)
	s.ok(tb, opPut, `{"TableName":"seed","Item":{"pk":{"S":"a"},"sk":{"N":"1"},"g":{"B":"AQ=="},`+
		`"m":{"M":{"l":{"L":[{"NULL":true},{"BS":["AQ=="]}]},"ns":{"NS":["1","2"]}}},"ss":{"SS":["x"]},"t":{"BOOL":true}}}`)
	s.ok(tb, opPut, `{"TableName":"seed","Item":{"pk":{"S":"a"},"sk":{"N":"2"}}}`)
	s.ok(tb, "UpdateTimeToLive", `{"TableName":"seed","TimeToLiveSpecification":{"Enabled":true,"AttributeName":"x"}}`)

	var buf bytes.Buffer
	require.NoError(tb, s.provider.SnapshotStreams(&buf))

	return snapshotOf(tb, s.provider), buf.Bytes()
}

// FuzzRestore feeds Restore any bytes. It must not panic, and a file it takes
// must give a state whose snapshot it takes again, to the same bytes.
func FuzzRestore(f *testing.F) {
	tables, _ := seedSnapshot(f)
	f.Add(tables)
	f.Add([]byte(fileWith(`{"meta":` + minimalMeta + `,"items":[{"id":{"S":"a"}}]}`)))

	f.Fuzz(func(t *testing.T, data []byte) {
		p := dynamodb.New(func() dynamodb.Settings { return dynamodb.Settings{} }, nil)
		if p.Restore(bytes.NewReader(data)) != nil {
			return
		}

		saved := snapshotOf(t, p)

		again := dynamodb.New(func() dynamodb.Settings { return dynamodb.Settings{} }, nil)
		require.NoError(t, again.Restore(bytes.NewReader(saved)))
		require.Equal(t, string(saved), string(snapshotOf(t, again)))
	})
}

// FuzzRestoreStreams feeds RestoreStreams any bytes over the restored seed
// tables. It must not panic, and the stream must still take records after.
func FuzzRestoreStreams(f *testing.F) {
	tables, streams := seedSnapshot(f)
	f.Add(streams)
	f.Add([]byte(`{"v":1,"shards":[]}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		s := newStack(t)
		require.NoError(t, s.provider.Restore(bytes.NewReader(tables)))

		_ = s.provider.RestoreStreams(bytes.NewReader(data))

		s.ok(t, opPut, `{"TableName":"seed","Item":{"pk":{"S":"after"},"sk":{"N":"1"}}}`)

		var buf bytes.Buffer
		require.NoError(t, s.provider.SnapshotStreams(&buf))
	})
}
