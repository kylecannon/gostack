package dynamodb_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/stream"
	"github.com/stretchr/testify/require"
)

// plainTable is streamTable without a stream: the pin tests write their items
// before the stream exists, so no record of those writes is kept.
const plainTable = `{"TableName":"streamed","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],` +
	`"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],"BillingMode":"PAY_PER_REQUEST"}`

// pinBound is the shard's byte bound in the pin tests, and what the heap the
// records hold may gain.
const (
	pinBound  = 4 << 20
	pinItems  = 200
	pinBlobKB = 50
)

// heapAfterWrites fills a table with pinItems items of pinBlobKB KB each,
// turns on a stream of viewType with a pinBound byte bound, and runs write
// for each item. It returns the heap the table and its stream hold afterwards,
// above what it held empty, and the shard's records.
func heapAfterWrites(t *testing.T, viewType string, write func(s *stack, i int)) (int64, []stream.Record) {
	t.Helper()

	s := newStack(t)
	s.ok(t, "CreateTable", plainTable)

	before := settledHeap()

	for i := range pinItems {
		s.ok(t, "PutItem", fmt.Sprintf(`{"TableName":"streamed","Item":{"id":{"S":"k%d"},"keep":{"S":"keep%d"},`+
			`"payload":{"S":"%d%s"}}}`, i, i, i, strings.Repeat("x", pinBlobKB<<10)))
	}

	s.ok(t, "UpdateTable", fmt.Sprintf(
		`{"TableName":"streamed","StreamSpecification":{"StreamEnabled":true,"StreamViewType":%q}}`, viewType))

	shard := dynamodb.Shard(s.provider, usEast1, "streamed")
	shard.SetLimits(stream.Retention, pinBound)
	t.Cleanup(shard.Stop)

	for i := range pinItems {
		write(s, i)
	}

	return settledHeap() - before, shard.Records()
}

// Test_stream_deleteKeysOnlyDoesNotPinItems: deleting items of 50 KB from a
// table whose stream is KEYS_ONLY leaves records of their keys alone. A key
// read from the deleted item's packed bytes would share them, and the
// record, charged a few hundred bytes, would pin all 50 KB of the item: 200
// deletes would hold 10 MB against a 4 MiB bound.
func Test_stream_deleteKeysOnlyDoesNotPinItems(t *testing.T) {
	gained, records := heapAfterWrites(t, "KEYS_ONLY", func(s *stack, i int) {
		s.ok(t, "DeleteItem", fmt.Sprintf(`{"TableName":"streamed","Key":{"id":{"S":"k%d"}}}`, i))
	})

	require.Len(t, records, pinItems, "every delete left a record, the bound trimmed none")
	require.LessOrEqual(t, gained, int64(pinBound), "the deletes' records hold no deleted item")
}

// Test_stream_updateNewImageDoesNotPinItems: an UpdateItem under NEW_IMAGE
// that drops the 50 KB attribute leaves records of the small item that is
// left. The new image built over the old item would share its strings' bytes,
// and the record would pin the old item whole: 200 updates would hold 10 MB
// against a 4 MiB bound. A record holds its new image as the packed item the
// table stored, which shares nothing with the old.
func Test_stream_updateNewImageDoesNotPinItems(t *testing.T) {
	gained, records := heapAfterWrites(t, "NEW_IMAGE", func(s *stack, i int) {
		s.ok(t, "UpdateItem", fmt.Sprintf(`{"TableName":"streamed","Key":{"id":{"S":"k%d"}},`+
			`"UpdateExpression":"REMOVE payload SET x = :v","ExpressionAttributeValues":{":v":{"N":"1"}}}`, i))
	})

	require.Len(t, records, pinItems, "every update left a record, the bound trimmed none")
	require.LessOrEqual(t, gained, int64(pinBound), "the updates' records hold no replaced item")
}
