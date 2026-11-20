package dynamodbstreams_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Test_replica_stream: a table replicated to a second region has its one
// stream in the owning region, which the replica region lists and reads with
// its own region in the ARN, as LocalStack shows it
// (test_streams_on_global_tables).
func Test_replica_stream(t *testing.T) {
	const usWest2 = "us-west-2"

	s := newStack(t)
	arn, _ := s.createStream(t, "global", "NEW_AND_OLD_IMAGES")
	s.ddbOK(t, "UpdateTable", `{"TableName":"global","ReplicaUpdates":[{"Create":{"RegionName":"`+usWest2+`"}}]}`)
	s.ddbOK(t, "BatchWriteItem", `{"RequestItems":{"global":[{"PutRequest":{"Item":{"id":{"S":"a"}}}},`+
		`{"PutRequest":{"Item":{"id":{"S":"b"}}}}]}}`)

	west := &stack{handler: s.handler, ddb: s.ddb, provider: s.provider, region: usWest2}

	westARN := at[string](t, west.ddbOK(t, "DescribeTable", `{"TableName":"global"}`), "Table", "LatestStreamArn")
	require.Equal(t, strings.Replace(arn, usEast1, usWest2, 1), westARN)

	streams := at[[]any](t, west.ok(t, "ListStreams", map[string]any{"TableName": "global"}), "Streams")
	require.Len(t, streams, 1)
	require.Equal(t, westARN, at[string](t, streams[0], keyStreamArn))

	described := west.ok(t, "DescribeStream", map[string]any{keyStreamArn: westARN})
	shard := at[string](t, at[[]any](t, described, "StreamDescription", "Shards")[0], keyShardID)

	for _, c := range []*stack{s, west} {
		streamARN := arn
		if c == west {
			streamARN = westARN
		}

		got := c.records(t, c.iterator(t, streamARN, shard, "TRIM_HORIZON"), 0).Records
		require.Len(t, got, 2, c.region)
	}
}
