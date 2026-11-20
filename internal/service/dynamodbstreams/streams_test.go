package dynamodbstreams_test

import (
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service/dynamodbstreams"
	"github.com/stretchr/testify/require"
)

// Test_routing: both services sign as dynamodb; X-Amz-Target picks each.
func Test_routing(t *testing.T) {
	s := newStack(t)

	require.Contains(t, s.ddbOK(t, "ListTables", `{}`), "TableNames")
	require.Equal(t, map[string]any{"Streams": []any{}}, s.ok(t, "ListStreams", `{}`))
}

// Test_getRecords_shape: the records of the batch-write snapshot
// (test_batch_write_items_streaming): four records in write order, an
// unchanged put leaving none, with LocalStack's fixed fields, the validated
// SizeBytes, and an eventID that is the same each time a record is read.
func Test_getRecords_shape(t *testing.T) {
	s := newStack(t)
	arn, shard := s.createStream(t, "streamed", "NEW_AND_OLD_IMAGES")
	it := s.iterator(t, arn, shard, "TRIM_HORIZON")

	s.ddbOK(t, "PutItem", `{"TableName":"streamed","Item":{"id":{"S":"Fred"}}}`)
	s.ddbOK(t, "BatchWriteItem", `{"RequestItems":{"streamed":[{"PutRequest":{"Item":{"id":{"S":"Fred"}}}},`+
		`{"PutRequest":{"Item":{"id":{"S":"NewKey"}}}}]}}`)
	s.ddbOK(t, "BatchWriteItem", `{"RequestItems":{"streamed":[{"DeleteRequest":{"Key":{"id":{"S":"NewKey"}}}},`+
		`{"PutRequest":{"Item":{"id":{"S":"Fred"},"name":{"S":"Fred"}}}}]}}`)

	got := s.records(t, it, 0)
	require.Len(t, got.Records, 4)

	type row struct {
		event, key string
		size       int64
		old, new   bool
	}

	rows := make([]row, len(got.Records))
	for i, r := range got.Records {
		require.Equal(t, "1.1", r.EventVersion)
		require.Equal(t, "aws:dynamodb", r.EventSource)
		require.Equal(t, usEast1, r.AwsRegion)
		require.Equal(t, "NEW_AND_OLD_IMAGES", r.Dynamodb.StreamViewType)
		require.Regexp(t, `^[0-9a-f]{8}$`, r.EventID)
		require.Regexp(t, `^\d{40}$`, r.Dynamodb.SequenceNumber)
		require.Nil(t, r.UserIdentity)
		require.Zero(t, r.Dynamodb.Approximate-float64(int64(r.Dynamodb.Approximate)), "whole seconds")

		if i > 0 {
			require.Greater(t, r.Dynamodb.SequenceNumber, got.Records[i-1].Dynamodb.SequenceNumber)
		}

		d := r.Dynamodb
		rows[i] = row{r.EventName, d.key(t), d.SizeBytes, d.OldImage != nil, d.NewImage != nil}
	}

	require.Equal(t, []row{
		{"INSERT", "Fred", 12, false, true},
		{"INSERT", "NewKey", 16, false, true},
		{"REMOVE", "NewKey", 16, true, false},
		{"MODIFY", "Fred", 26, true, true},
	}, rows)

	again := s.records(t, s.iterator(t, arn, shard, "TRIM_HORIZON"), 1)
	require.Len(t, again.Records, 1)
	require.Equal(t, got.Records[0].EventID, again.Records[0].EventID)
	require.Equal(t, got.Records[0].Dynamodb.SequenceNumber, again.Records[0].Dynamodb.SequenceNumber)

	require.NotNil(t, got.NextShardIterator)
	empty := s.records(t, *got.NextShardIterator, 0)
	require.Empty(t, empty.Records)
	require.NotNil(t, empty.NextShardIterator, "an open shard always has a next iterator")
}

// Test_describeStream: the shard id and iterator in the forms LocalStack shows
// clients (test_dynamodb_streams_shard_iterator_format), ExclusiveStartShardId
// leaving the one shard out, and a disabled stream's closed shard, read to its
// end, giving no next iterator.
func Test_describeStream(t *testing.T) {
	s := newStack(t)
	arn, shard := s.createStream(t, "streamed", "KEYS_ONLY")

	require.Regexp(t, `^shardId-\d{20}-000000000000$`, shard)

	described := at[map[string]any](t, s.ok(t, "DescribeStream", map[string]any{keyStreamArn: arn}), "StreamDescription")
	require.Equal(t, "ENABLED", described["StreamStatus"])
	require.Equal(t, "KEYS_ONLY", described["StreamViewType"])
	require.Equal(t, "streamed", described["TableName"])
	require.Equal(t, []any{map[string]any{"AttributeName": "id", "KeyType": "HASH"}}, described["KeySchema"])
	require.Contains(t, described, "CreationRequestDateTime")

	excluded := s.ok(t, "DescribeStream", map[string]any{keyStreamArn: arn, "ExclusiveStartShardId": shard})
	require.Equal(t, []any{}, at[[]any](t, excluded, "StreamDescription", "Shards"))

	it := s.iterator(t, arn, shard, "TRIM_HORIZON")
	require.Regexp(t, "^"+regexp.QuoteMeta(arn)+`\|\d\|.+$`, it)

	s.ddbOK(t, "PutItem", `{"TableName":"streamed","Item":{"id":{"S":"a"}}}`)
	s.ddbOK(t, "UpdateTable", `{"TableName":"streamed","StreamSpecification":{"StreamEnabled":false}}`)
	s.ddbOK(t, "PutItem", `{"TableName":"streamed","Item":{"id":{"S":"b"}}}`)

	described = at[map[string]any](t, s.ok(t, "DescribeStream", map[string]any{keyStreamArn: arn}), "StreamDescription")
	require.Equal(t, "DISABLED", described["StreamStatus"])

	sequence := at[map[string]any](t, at[[]any](t, described, "Shards")[0], "SequenceNumberRange")
	require.Contains(t, sequence, "EndingSequenceNumber")

	got := s.records(t, it, 0)
	require.Len(t, got.Records, 1, "a closed shard takes no record")
	require.Equal(t, sequence["EndingSequenceNumber"], got.Records[0].Dynamodb.SequenceNumber)
	require.Nil(t, got.NextShardIterator, "a closed shard read to its end has no next iterator")

	streams := at[[]any](t, s.ok(t, "ListStreams", `{}`), "Streams")
	require.Len(t, streams, 1, "a disabled stream is still listed")
}

// Test_describeStream_notFound: test_non_existent_stream's error, naming the
// caller's account and region in the ARN as given.
func Test_describeStream_notFound(t *testing.T) {
	s := newStack(t)
	arn := "arn:aws:dynamodb:us-east-1:000000000000:table/non-existent/stream/2024-11-18T14:36:44.149"

	got := s.call(t, streamsTarget, "DescribeStream", `{"StreamArn":"`+arn+`"}`)
	require.Equal(t, 400, got.status)
	require.JSONEq(t, `{"__type":"ResourceNotFoundException",`+
		`"message":"Requested resource not found: Stream: `+arn+` not found"}`, got.body)

	s.ddbOK(t, "CreateTable", tableBody("plain", ""))
	s.fail(t, "DescribeStream", map[string]any{keyStreamArn: arn[:len(arn)-1] + "0"}, "ResourceNotFoundException")
}

// Test_shardIteratorTypes: each iterator type starts where AWS documents,
// with the shard's StartingSequenceNumber read as the position before the
// first record.
func Test_shardIteratorTypes(t *testing.T) {
	s := newStack(t)
	arn, shard := s.createStream(t, "streamed", "NEW_IMAGE")

	for i := range 3 {
		s.ddbOK(t, "PutItem", fmt.Sprintf(`{"TableName":"streamed","Item":{"id":{"S":"k%d"}}}`, i))
	}

	all := s.records(t, s.iterator(t, arn, shard, "TRIM_HORIZON"), 0).Records
	require.Len(t, all, 3)

	latest := s.records(t, s.iterator(t, arn, shard, "LATEST"), 0)
	require.Empty(t, latest.Records)

	s.ddbOK(t, "PutItem", `{"TableName":"streamed","Item":{"id":{"S":"k3"}}}`)
	require.Len(t, s.records(t, *latest.NextShardIterator, 0).Records, 1, "LATEST sees what follows it")

	described := at[map[string]any](t, s.ok(t, "DescribeStream", map[string]any{keyStreamArn: arn}), "StreamDescription")
	start := at[string](t, at[[]any](t, described, "Shards")[0], "SequenceNumberRange", "StartingSequenceNumber")

	from := func(typ string, seq any) []string {
		out := s.ok(t, "GetShardIterator", map[string]any{
			keyStreamArn: arn, keyShardID: shard, keyIteratorType: typ, "SequenceNumber": seq,
		})

		page := s.records(t, at[string](t, out, keyShardIterator), 0)

		keys := make([]string, 0, len(page.Records))
		for _, r := range page.Records {
			keys = append(keys, r.Dynamodb.key(t))
		}

		return keys
	}

	require.Equal(t, []string{"k0", "k1", "k2", "k3"}, from("AT_SEQUENCE_NUMBER", start))
	require.Equal(t, []string{"k1", "k2", "k3"}, from("AT_SEQUENCE_NUMBER", all[1].Dynamodb.SequenceNumber))
	require.Equal(t, []string{"k2", "k3"}, from("AFTER_SEQUENCE_NUMBER", all[1].Dynamodb.SequenceNumber))

	s.fail(t, "GetShardIterator", map[string]any{
		keyStreamArn: arn, keyShardID: shard, keyIteratorType: "AT_SEQUENCE_NUMBER",
	}, "ValidationException")
	s.fail(t, "GetShardIterator", map[string]any{
		keyStreamArn: arn, keyShardID: shard, keyIteratorType: "AT_SEQUENCE_NUMBER",
		"SequenceNumber": "123456789012345678901",
	}, "ValidationException")
	s.fail(t, "GetShardIterator", map[string]any{
		keyStreamArn: arn, keyShardID: "shardId-00000000000000000000-000000000000",
		keyIteratorType: "LATEST",
	}, "ResourceNotFoundException")
}

// Test_validation: the model's constraints, one ValidationException each.
func Test_validation(t *testing.T) {
	s := newStack(t)
	arn, shard := s.createStream(t, "streamed", "NEW_IMAGE")
	it := s.iterator(t, arn, shard, "LATEST")

	for _, tc := range []struct {
		operation string
		in        any
		msg       string
	}{
		{
			opGetRecords,
			map[string]any{keyShardIterator: it, keyLimit: 0},
			"1 validation error detected: Value '0' at 'limit' failed to satisfy constraint: " +
				"Member must have value greater than or equal to 1",
		},
		{
			opGetRecords,
			map[string]any{keyShardIterator: it, keyLimit: 1001},
			"1 validation error detected: Value '1001' at 'limit' failed to satisfy constraint: " +
				"Member must have value less than or equal to 1000",
		},
		{opGetRecords, `{}`, "1 validation error detected: Value null at 'shardIterator' failed to satisfy constraint: " +
			"Member must not be null"},
		{opGetRecords, map[string]any{keyShardIterator: arn + "|1|bogus"}, "Invalid ShardIterator"},
		{
			"GetShardIterator",
			map[string]any{keyStreamArn: arn, keyShardID: shard, keyIteratorType: "BOGUS"},
			"1 validation error detected: Value 'BOGUS' at 'shardIteratorType' failed to satisfy constraint: " +
				"Member must satisfy enum value set: [TRIM_HORIZON, LATEST, AT_SEQUENCE_NUMBER, AFTER_SEQUENCE_NUMBER]",
		},
		{"GetShardIterator", `{}`, "3 validation errors detected: " +
			"Value null at 'streamArn' failed to satisfy constraint: Member must not be null; " +
			"Value null at 'shardId' failed to satisfy constraint: Member must not be null; " +
			"Value null at 'shardIteratorType' failed to satisfy constraint: Member must not be null"},
		{
			"ListStreams",
			map[string]any{keyLimit: 101},
			"1 validation error detected: Value '101' at 'limit' failed to satisfy constraint: " +
				"Member must have value less than or equal to 100",
		},
		{"DescribeStream", `{}`, "1 validation error detected: Value null at 'streamArn' failed to satisfy constraint: " +
			"Member must not be null"},
	} {
		require.Equal(t, tc.msg, s.fail(t, tc.operation, tc.in, "ValidationException"), tc.operation)
	}
}

// Test_listStreams: oldest first, filtered by TableName, paged by Limit and
// ExclusiveStartStreamArn.
func Test_listStreams(t *testing.T) {
	s := newStack(t)
	first, _ := s.createStream(t, "one", "KEYS_ONLY")
	second, _ := s.createStream(t, "two", "KEYS_ONLY")
	s.ddbOK(t, "CreateTable", tableBody("none", ""))

	arns := func(out map[string]any) []string {
		streams := at[[]any](t, out, "Streams")

		got := make([]string, 0, len(streams))
		for _, stream := range streams {
			got = append(got, at[string](t, stream, keyStreamArn))
		}

		return got
	}

	require.Equal(t, []string{first, second}, arns(s.ok(t, "ListStreams", `{}`)))
	require.Equal(t, []string{second}, arns(s.ok(t, "ListStreams", map[string]any{"TableName": "two"})))

	paged := s.ok(t, "ListStreams", map[string]any{keyLimit: 1})
	require.Equal(t, []string{first}, arns(paged))
	require.Equal(t, first, paged["LastEvaluatedStreamArn"])

	rest := s.ok(t, "ListStreams", map[string]any{keyLimit: 1, "ExclusiveStartStreamArn": first})
	require.Equal(t, []string{second}, arns(rest))
	require.NotContains(t, rest, "LastEvaluatedStreamArn")
}

// Test_iterator_expiry: an iterator is refused 15 minutes after it was
// issued, and the next iterator GetRecords hands out starts its own 15.
func Test_iterator_expiry(t *testing.T) {
	s := newStack(t)
	clock := time.Now()
	dynamodbstreams.SetClock(s.provider, func() time.Time { return clock })

	arn, shard := s.createStream(t, "streamed", "NEW_IMAGE")
	it := s.iterator(t, arn, shard, "TRIM_HORIZON")

	clock = clock.Add(14 * time.Minute)
	next := s.records(t, it, 0).NextShardIterator
	require.NotNil(t, next)

	clock = clock.Add(2 * time.Minute)
	require.Equal(t, "Shard iterator has expired", s.fail(t, opGetRecords, map[string]any{keyShardIterator: it},
		"ExpiredIteratorException"))
	require.Empty(t, s.records(t, *next, 0).Records)
}

// Test_trimmed: an iterator at a record the byte bound trimmed is refused with
// TrimmedDataAccessException, and TRIM_HORIZON then starts at the oldest
// record kept.
func Test_trimmed(t *testing.T) {
	s := newStack(t)
	arn, shard := s.createStream(t, "streamed", "NEW_IMAGE")

	info, ok := s.ddb.StreamByARN(request.With(t.Context(), ctx(t)), arn)
	require.True(t, ok)
	// The test narrows the shard's byte bound, a write-side knob the reader's
	// interface does not carry.
	limits, ok := info.Shard.(interface {
		SetLimits(retention time.Duration, maxBytes int64)
	})
	require.True(t, ok)
	limits.SetLimits(time.Hour, 3*300)

	old := s.iterator(t, arn, shard, "TRIM_HORIZON")

	for i := range 5 {
		s.ddbOK(t, "PutItem", fmt.Sprintf(`{"TableName":"streamed","Item":{"id":{"S":"k%d"}}}`, i))
	}

	s.fail(t, opGetRecords, map[string]any{keyShardIterator: old}, "TrimmedDataAccessException")

	kept := s.records(t, s.iterator(t, arn, shard, "TRIM_HORIZON"), 0).Records
	require.Len(t, kept, 3)
	require.Equal(t, "k2", kept[0].Dynamodb.key(t))
}
