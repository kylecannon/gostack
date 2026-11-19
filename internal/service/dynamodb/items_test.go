package dynamodb_test

import (
	"net/http"
	"testing"

	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/stretchr/testify/require"
)

const (
	opPut     = "PutItem"
	opGet     = "GetItem"
	opDeleteI = "DeleteItem"
	keyAttrs  = "Attributes"
	keyItem   = "Item"
	keyUnits  = "CapacityUnits"
	getID1    = `{"TableName":"tab1","Key":{"id":{"S":"id1"}}}`
)

// Test_putItem_returnValues is test_return_values_in_put_item: ALL_OLD
// answers the item replaced, and nothing when there was none.
func Test_putItem_returnValues(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("tab1"))

	put := func(data, returnValues string) map[string]any {
		return s.ok(t, opPut, `{"TableName":"tab1","Item":{"id":{"S":"id1"},"data":{"S":"`+data+`"}}`+returnValues+`}`)
	}
	allOld := `,"ReturnValues":"ALL_OLD"`
	old := map[string]any{"id": map[string]any{"S": "id1"}, "data": map[string]any{"S": "foobar"}}

	require.Empty(t, put("foobar", allOld))
	require.Equal(t, old, put("foobar", allOld)[keyAttrs])
	require.Equal(t, old, put("barfoo", allOld)[keyAttrs])
	require.Empty(t, put("foobar", ""))
	require.Empty(t, put("foobar", `,"ReturnValues":"NONE"`))

	s.fails(t, opPut, `{"TableName":"tab1","Item":{"id":{"S":"x"}},"ReturnValues":"ALL_NEW"}`, codeValidation,
		"Return values set to invalid value")
}

// Test_items_roundTrip is test_empty_and_binary_values and
// test_non_ascii_chars: what is put is what is read back.
func Test_items_roundTrip(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("tab1"))

	item := `{"id":{"S":"id1"},"empty":{"S":""},"bin":{"B":"kA=="},"text":{"S":"foobar123 ✓ £ ¢"},` +
		`"n":{"N":"1.50"},"ss":{"SS":["a","b"]},"m":{"M":{"l":{"L":[{"NULL":true},{"BOOL":false}]}}}}`
	require.Empty(t, s.ok(t, opPut, `{"TableName":"tab1","Item":`+item+`}`))

	got := s.ok(t, opGet, getID1)[keyItem]
	require.Equal(t, "foobar123 ✓ £ ¢", path(t, got, "text", "S"))
	require.Equal(t, "kA==", path(t, got, "bin", "B"))
	require.Empty(t, path(t, got, "empty", "S"))
	require.Equal(t, "1.5", path(t, got, "n", "N"))
	require.Equal(t, false, path(t, got, "m", "M", "l", "L", 1, "BOOL"))

	deleted := s.ok(t, opDeleteI, `{"TableName":"tab1","Key":{"id":{"S":"id1"}},"ReturnValues":"ALL_OLD"}`)
	require.Equal(t, got, deleted[keyAttrs])
	require.Empty(t, s.ok(t, opGet, getID1))
	require.Empty(t, s.ok(t, opDeleteI, getID1))
}

// Test_items_errors holds the key and item messages, and the 501 for the
// expressions later changes bring.
func Test_items_errors(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("tab1"))

	tests := []struct {
		name, op, body, code, message string
	}{
		{
			"missing table", opPut, `{"TableName":"nope","Item":{"id":{"S":"a"}}}`, codeNotFound,
			"Cannot do operations on a non-existent table",
		},
		{"null item", opPut, `{"TableName":"tab1"}`, codeValidation, "Item to put in PutItem cannot be null"},
		{
			"null key", opGet, `{"TableName":"tab1"}`, codeValidation,
			"Cannot have null key for GetItem, DeleteItem, or UpdateItem",
		},
		{
			"key type", opGet, `{"TableName":"tab1","Key":{"id":{"N":"1"}}}`, codeValidation,
			"One or more parameter values were invalid: Type mismatch for key",
		},
		{
			"extra key", opDeleteI, `{"TableName":"tab1","Key":{"id":{"S":"a"},"x":{"S":"b"}}}`, codeValidation,
			"The number of conditions on the keys is invalid",
		},
		{
			"missing key", opPut, `{"TableName":"tab1","Item":{"x":{"S":"a"}}}`, codeValidation,
			"One of the required keys was not given a value",
		},
		{
			"empty value", opPut, `{"TableName":"tab1","Item":{"id":{"S":"a"},"x":{}}}`, codeValidation,
			"Supplied AttributeValue is empty, must contain exactly one of the supported datatypes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s.fails(t, tt.op, tt.body, tt.code, tt.message)
		})
	}

	for _, body := range []string{
		`{"TableName":"tab1","Item":{"id":{"S":"a"}},"ConditionExpression":"attribute_exists(id)"}`,
		`{"TableName":"tab1","Item":{"id":{"S":"a"}},"Expected":{"id":{"Exists":false}}}`,
	} {
		require.Equal(t, http.StatusNotImplemented, s.call(t, usEast1, opPut, body).status)
	}

	require.Equal(t, http.StatusNotImplemented,
		s.call(t, usEast1, opGet, `{"TableName":"tab1","Key":{"id":{"S":"a"}},"ProjectionExpression":"id"}`).status)
}

// Test_items_consumedCapacity: TOTAL and INDEXES report the units the call
// used.
func Test_items_consumedCapacity(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("tab1"))

	put := s.ok(t, opPut, `{"TableName":"tab1","Item":{"id":{"S":"id1"}},"ReturnConsumedCapacity":"TOTAL"}`)
	require.Equal(t, map[string]any{"TableName": "tab1", keyUnits: 1.0}, put["ConsumedCapacity"])

	get := s.ok(t, opGet, `{"TableName":"tab1","Key":{"id":{"S":"id1"}},"ReturnConsumedCapacity":"INDEXES"}`)
	require.Equal(t, map[string]any{"TableName": "tab1", keyUnits: 0.5, "Table": map[string]any{keyUnits: 0.5}},
		get["ConsumedCapacity"])
}

// Test_throttling: DYNAMODB_READ_ERROR_PROBABILITY 1 throttles item reads and
// leaves writes and table operations alone.
func Test_throttling(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("tab1"))
	s.settings.Store(&dynamodb.Settings{ReadErrorProbability: 1})

	s.fails(t, opGet, getID1, "ProvisionedThroughputExceededException",
		"The level of configured provisioned throughput for the table was exceeded. "+
			"Consider increasing your provisioning level with the UpdateTable API")
	s.ok(t, opPut, `{"TableName":"tab1","Item":{"id":{"S":"id1"}}}`)
	s.ok(t, opDescribe, describeTab1)
}

// Test_shareDB: DYNAMODB_SHARE_DB gives every region one set of tables, with
// ARNs in the caller's region.
func Test_shareDB(t *testing.T) {
	s := newStack(t)
	s.settings.Store(&dynamodb.Settings{ShareDB: true})
	s.ok(t, opCreate, hashTable("tab1"))

	got := s.call(t, "eu-west-1", opDescribe, describeTab1)
	require.Equal(t, http.StatusOK, got.status, got.body)
	require.Contains(t, got.body, "arn:aws:dynamodb:eu-west-1:")
}

// Test_mutating: reads do not mark the service dirty.
func Test_mutating(t *testing.T) {
	mutating := dynamodb.New(nil).Registration().Mutating

	for _, op := range []string{opGet, "Query", "Scan", "BatchGetItem", "TransactGetItems", opDescribe, opList} {
		require.False(t, mutating(op), op)
	}

	for _, op := range []string{opPut, opDeleteI, "UpdateItem", opCreate, opUpdate, "BatchWriteItem"} {
		require.True(t, mutating(op), op)
	}
}
