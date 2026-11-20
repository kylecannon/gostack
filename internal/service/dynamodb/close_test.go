package dynamodb_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/stream"
	"github.com/stretchr/testify/require"
)

// Test_close_stopsShardTimers closes the provider while a stream shard holds
// a record and its retention timer is armed: Close disarms it, no timer
// trims the record after Close, a record written after Close arms none, and
// the streams snapshot, which the last save takes after Close and after the
// table snapshot, still holds the records.
func Test_close_stopsShardTimers(t *testing.T) {
	const retention = 30 * time.Millisecond

	s := newStack(t)
	s.ok(t, opCreate, `{"TableName":"closing","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],`+
		`"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],"BillingMode":"PAY_PER_REQUEST",`+
		`"StreamSpecification":{"StreamEnabled":true,"StreamViewType":"KEYS_ONLY"}}`)

	shard := dynamodb.Shard(s.provider, usEast1, "closing")
	shard.SetLimits(retention, stream.MaxBytes)

	s.ok(t, opPut, `{"TableName":"closing","Item":{"id":{"S":"a"}}}`)
	require.True(t, shard.Armed())

	s.provider.Registration().Close(t.Context())
	require.False(t, shard.Armed(), "Close disarms the shard's timer")

	s.ok(t, opPut, `{"TableName":"closing","Item":{"id":{"S":"b"}}}`)
	require.False(t, shard.Armed(), "a record written after Close arms no timer")

	require.Never(t, func() bool { return len(shard.Records()) < 2 }, 10*retention, retention/3,
		"no timer trims a record after Close")

	// The last save takes the table snapshot, then the streams one.
	savedOf(t, s.provider)

	var buf bytes.Buffer
	require.NoError(t, s.provider.SnapshotStreams(&buf))
	require.Contains(t, buf.String(), `"seq":2`)
}
