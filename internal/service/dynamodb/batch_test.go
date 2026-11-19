package dynamodb_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/stretchr/testify/require"
)

const (
	opBatchWrite = "BatchWriteItem"
	opBatchGet   = "BatchGetItem"

	msgNoTable       = "Cannot do operations on a non-existent table"
	msgKeyNotGiven   = "One of the required keys was not given a value"
	caseMissingTable = "missing table"
)

// rangeKeyTable is a CreateTable body for a provisioned table keyed by id and
// sk.
func rangeKeyTable(name string) string {
	return `{"TableName":"` + name + `","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},` +
		`{"AttributeName":"sk","KeyType":"RANGE"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"},` +
		`{"AttributeName":"sk","AttributeType":"S"}],"ProvisionedThroughput":{"ReadCapacityUnits":5,"WriteCapacityUnits":5}}`
}

// Test_batchWrite_rows: the bodies test_batch_write_items,
// test_batch_write_binary and test_batch_write_not_matching_schema pin, an
// empty UnprocessedItems included, and the writes taking effect.
func Test_batchWrite_rows(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("tab1"))
	s.ok(t, opPut, `{"TableName":"tab1","Item":{"id":{"S":"Fred"}}}`)

	got := s.call(t, usEast1, opBatchWrite, `{"RequestItems":{"tab1":[{"DeleteRequest":{"Key":{"id":{"S":"Fred"}}}},`+
		`{"PutRequest":{"Item":{"id":{"S":"Bob"},"data":{"B":"dGVzdCDAIO0="}}}}]}}`)
	require.Equal(t, http.StatusOK, got.status, got.body)
	require.JSONEq(t, `{"UnprocessedItems":{}}`, got.body)

	require.JSONEq(t, `{}`, s.call(t, usEast1, opGet, `{"TableName":"tab1","Key":{"id":{"S":"Fred"}}}`).body)
	require.JSONEq(t, `{"Item":{"id":{"S":"Bob"},"data":{"B":"dGVzdCDAIO0="}}}`,
		s.call(t, usEast1, opGet, `{"TableName":"tab1","Key":{"id":{"S":"Bob"}}}`).body)

	s.ok(t, opCreate, rangeKeyTable("tab2"))
	s.fails(t, opBatchWrite, `{"RequestItems":{"tab2":[{"PutRequest":{"Item":{"nonKey":{"S":"hello"}}}}]}}`,
		codeValidation, "The provided key element does not match the schema")
}

// Test_batchWrite_refusals: each message DynamoDB Local answers, probed on the
// LocalStack image, in its order of checks.
func Test_batchWrite_refusals(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("tab1"))

	puts := func(n int) string {
		list := make([]string, n)
		for i := range list {
			list[i] = fmt.Sprintf(`{"PutRequest":{"Item":{"id":{"S":"%d"}}}}`, i)
		}

		return strings.Join(list, ",")
	}

	for _, c := range []struct {
		name, body, code, message string
	}{
		{
			"26 requests", `{"RequestItems":{"tab1":[` + puts(26) + `]}}`, codeValidation,
			"Too many items requested for the BatchWriteItem call",
		},
		{
			"26 on a missing table", `{"RequestItems":{"nope":[` + puts(26) + `]}}`, codeValidation,
			"Too many items requested for the BatchWriteItem call",
		},
		{"no requests", `{"RequestItems":{}}`, codeValidation, "BatchWriteItem cannot have a null or no requests set"},
		{"no RequestItems", `{}`, codeValidation, "BatchWriteItem cannot have a null or no requests set"},
		{
			"empty list", `{"RequestItems":{"tab1":[]}}`, codeValidation,
			"The batch write request list for a table cannot be null or empty: tab1",
		},
		{
			caseMissingTable, `{"RequestItems":{"nope":[` + puts(1) + `]}}`, codeNotFound,
			msgNoTable,
		},
		{
			// nope sorts before tab1 in Java's HashMap, so its absence wins.
			"missing table before a bad item", `{"RequestItems":{"tab1":[{"PutRequest":{"Item":{"x":{"S":"1"}}}}],` +
				`"nope":[` + puts(1) + `]}}`, codeNotFound, msgNoTable,
		},
		{
			"empty request", `{"RequestItems":{"tab1":[{}]}}`, codeValidation,
			"Supplied AttributeValue has more than one datatypes set, must contain exactly one of the supported datatypes",
		},
		{
			"null item", `{"RequestItems":{"tab1":[{"PutRequest":{}}]}}`, codeValidation,
			"The supplied Item contains a null AttributeValue",
		},
		{
			"key type", `{"RequestItems":{"tab1":[{"PutRequest":{"Item":{"id":{"N":"1"}}}}]}}`, codeValidation,
			"One or more parameter values were invalid: Type mismatch for key",
		},
		{
			"extra key", `{"RequestItems":{"tab1":[{"DeleteRequest":{"Key":{"id":{"S":"1"},"x":{"S":"1"}}}}]}}`,
			codeValidation, "The number of conditions on the keys is invalid",
		},
		{"duplicate", `{"RequestItems":{"tab1":[{"PutRequest":{"Item":{"id":{"S":"1"}}}},` +
			`{"DeleteRequest":{"Key":{"id":{"S":"1"}}}}]}}`, codeValidation, "Provided list of item keys contains duplicates"},
		{"bad item before duplicate", `{"RequestItems":{"tab1":[{"PutRequest":{"Item":{"x":{"S":"1"}}}},` +
			puts(1) + `,` + puts(1) + `]}}`, codeValidation, "The provided key element does not match the schema"},
		{
			"bad number", `{"RequestItems":{"tab1":[{"PutRequest":{"Item":{"id":{"S":"1"},"n":{"N":"abc"}}}}]}}`,
			codeValidation, "A value provided cannot be converted into a number",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			s.fails(t, opBatchWrite, c.body, c.code, c.message)
		})
	}

	// Nothing a refused request named was written.
	require.JSONEq(t, `{}`, s.call(t, usEast1, opGet, `{"TableName":"tab1","Key":{"id":{"S":"0"}}}`).body)
}

// Test_batchWrite_capacity: ConsumedCapacity a table at a time in DynamoDB
// Local's order (bw2 before bw1), INDEXES with the table's own units.
func Test_batchWrite_capacity(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("bw1"))
	s.ok(t, opCreate, rangeKeyTable("bw2"))

	got := s.call(t, usEast1, opBatchWrite, `{"ReturnConsumedCapacity":"TOTAL","RequestItems":{`+
		`"bw1":[{"PutRequest":{"Item":{"id":{"S":"1"}}}}],"bw2":[{"PutRequest":{"Item":{"id":{"S":"a"},"sk":{"S":"b"}}}}]}}`)
	require.JSONEq(t, `{"UnprocessedItems":{},"ConsumedCapacity":[{"TableName":"bw2","CapacityUnits":1},`+
		`{"TableName":"bw1","CapacityUnits":1}]}`, got.body)

	got = s.call(t, usEast1, opBatchWrite, `{"ReturnConsumedCapacity":"INDEXES","ReturnItemCollectionMetrics":"SIZE",`+
		`"RequestItems":{"bw1":[{"PutRequest":{"Item":{"id":{"S":"1"}}}},{"PutRequest":{"Item":{"id":{"S":"3"}}}}]}}`)
	require.JSONEq(t, `{"UnprocessedItems":{},"ConsumedCapacity":[{"TableName":"bw1","CapacityUnits":2,`+
		`"Table":{"CapacityUnits":2}}]}`, got.body)
}

// Test_batchWrite_throttled: under DYNAMODB_WRITE_ERROR_PROBABILITY 1 the
// whole call is refused, as LocalStack's provider refuses it.
func Test_batchWrite_throttled(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("tab1"))
	s.settings.Store(&dynamodb.Settings{Host: testHost, WriteErrorProbability: 1})

	s.fails(t, opBatchWrite, `{"RequestItems":{"tab1":[{"PutRequest":{"Item":{"id":{"S":"1"}}}}]}}`,
		"ProvisionedThroughputExceededException", "The level of configured provisioned throughput for the table "+
			"was exceeded. Consider increasing your provisioning level with the UpdateTable API")
}

// Test_batchGet_rows: the body test_dynamodb_get_batch_items pins, every table
// listed with what was found, projected, and the consumed capacity of found
// items only.
func Test_batchGet_rows(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("bw1"))
	s.ok(t, opCreate, rangeKeyTable("bw2"))

	got := s.call(t, usEast1, opBatchGet, `{"RequestItems":{"bw1":{"Keys":[{"id":{"S":"test-key"}}]}}}`)
	require.Equal(t, http.StatusOK, got.status, got.body)
	require.JSONEq(t, `{"Responses":{"bw1":[]},"UnprocessedKeys":{}}`, got.body)

	s.ok(t, opPut, `{"TableName":"bw1","Item":{"id":{"S":"1"},"a":{"S":"x"},"b":{"N":"1"}}}`)
	s.ok(t, opPut, `{"TableName":"bw1","Item":{"id":{"S":"2"},"a":{"S":"x"}}}`)
	s.ok(t, opPut, `{"TableName":"bw2","Item":{"id":{"S":"1"},"sk":{"S":"s"}}}`)

	got = s.call(t, usEast1, opBatchGet, `{"ReturnConsumedCapacity":"TOTAL","RequestItems":{`+
		`"bw1":{"Keys":[{"id":{"S":"1"}},{"id":{"S":"2"}},{"id":{"S":"99"}}],"ProjectionExpression":"#a,id",`+
		`"ExpressionAttributeNames":{"#a":"a"}},`+
		`"bw2":{"Keys":[{"id":{"S":"1"},"sk":{"S":"s"}}],"ConsistentRead":true}}}`)
	require.JSONEq(t, `{"Responses":{"bw1":[{"id":{"S":"1"},"a":{"S":"x"}},{"id":{"S":"2"},"a":{"S":"x"}}],`+
		`"bw2":[{"id":{"S":"1"},"sk":{"S":"s"}}]},"UnprocessedKeys":{},`+
		`"ConsumedCapacity":[{"TableName":"bw2","CapacityUnits":1},{"TableName":"bw1","CapacityUnits":1}]}`, got.body)

	got = s.call(t, usEast1, opBatchGet, `{"ReturnConsumedCapacity":"INDEXES","RequestItems":{`+
		`"bw1":{"Keys":[{"id":{"S":"99"}}]}}}`)
	require.JSONEq(t, `{"Responses":{"bw1":[]},"UnprocessedKeys":{},`+
		`"ConsumedCapacity":[{"TableName":"bw1","CapacityUnits":0,"Table":{"CapacityUnits":0}}]}`, got.body)
}

// Test_batchGet_refusals: each message DynamoDB Local answers, probed on the
// LocalStack image, in its order of checks.
func Test_batchGet_refusals(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("tab1"))

	keys := func(n int) string {
		list := make([]string, n)
		for i := range list {
			list[i] = fmt.Sprintf(`{"id":{"S":"%d"}}`, i)
		}

		return strings.Join(list, ",")
	}

	for _, c := range []struct {
		name, body, code, message string
	}{
		{
			"101 keys", `{"RequestItems":{"tab1":{"Keys":[` + keys(101) + `]}}}`, codeValidation,
			"Too many items requested for the BatchGetItem call",
		},
		{
			"101 on a missing table", `{"RequestItems":{"nope":{"Keys":[` + keys(101) + `]}}}`, codeValidation,
			"Too many items requested for the BatchGetItem call",
		},
		{"no requests", `{"RequestItems":{}}`, codeValidation, "BatchGetItem must have some requests set"},
		{"no RequestItems", `{}`, codeValidation, "BatchGetItem must have some requests set"},
		{
			"no keys", `{"RequestItems":{"tab1":{}}}`, codeValidation,
			"The list of keys in RequestItems for BatchGetItem is required: tab1 has empty list",
		},
		{
			"bad name", `{"RequestItems":{"a":{"Keys":[` + keys(1) + `]}}}`, codeValidation,
			"Invalid table/index name.  Table/index names must be between 3 and 255 characters long, " +
				"and may contain only the characters a-z, A-Z, 0-9, '_', '-', and '.'",
		},
		{
			caseMissingTable, `{"RequestItems":{"nope":{"Keys":[` + keys(1) + `]}}}`, codeNotFound,
			msgNoTable,
		},
		{
			"empty key", `{"RequestItems":{"tab1":{"Keys":[{}]}}}`, codeValidation,
			"The list of keys in RequestItems for BatchGetItem may not contain null or empty values: " +
				"tab1 has one or more empty keys",
		},
		{"duplicate after a bad key", `{"RequestItems":{"tab1":{"Keys":[{"x":{"S":"1"}},` + keys(1) + `,` + keys(1) +
			`]}}}`, codeValidation, "Provided list of item keys contains duplicates"},
		{
			"missing key", `{"RequestItems":{"tab1":{"Keys":[{"x":{"S":"1"}}]}}}`, codeValidation,
			msgKeyNotGiven,
		},
		{"bad key before bad projection", `{"RequestItems":{"tab1":{"Keys":[{"x":{"S":"1"}}],` +
			`"ProjectionExpression":"a,,"}}}`, codeValidation, msgKeyNotGiven},
		{
			"bad projection", `{"RequestItems":{"tab1":{"Keys":[` + keys(1) + `],"ProjectionExpression":"a,,"}}}`,
			codeValidation, `Invalid ProjectionExpression: Syntax error; token: ",", near: ",,"`,
		},
		{
			"names with no projection", `{"RequestItems":{"tab1":{"Keys":[` + keys(1) +
				`],"ExpressionAttributeNames":{"#a":"a"}}}}`, codeValidation,
			"ExpressionAttributeNames can only be specified when using expressions",
		},
		{
			"unused name", `{"RequestItems":{"tab1":{"Keys":[` + keys(1) +
				`],"ProjectionExpression":"a","ExpressionAttributeNames":{"#a":"a"}}}}`, codeValidation,
			"Value provided in ExpressionAttributeNames unused in expressions: keys: {#a}",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			s.fails(t, opBatchGet, c.body, c.code, c.message)
		})
	}

	s.ok(t, opBatchGet, `{"RequestItems":{"tab1":{"Keys":[`+keys(100)+`]}}}`)
}

// Test_batchGet_sizeCap: the read stops before the item that would take it
// past 16 MB of stored bytes, projection or not, and returns the rest of the
// keys with the table's request in UnprocessedKeys; a table read after the
// cap is wholly unprocessed.
func Test_batchGet_sizeCap(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("tab1"))

	big := strings.Repeat("x", 390000)
	keys := make([]string, 50)

	for i := range keys {
		keys[i] = fmt.Sprintf(`{"id":{"S":"big%d"}}`, i)
		s.ok(t, opPut, fmt.Sprintf(`{"TableName":"tab1","Item":{"id":{"S":"big%d"},"d":{"S":"%s"}}}`, i, big))
	}

	out := s.ok(t, opBatchGet, `{"ReturnConsumedCapacity":"TOTAL","RequestItems":{"tab1":{"Keys":[`+
		strings.Join(keys, ",")+`],"ProjectionExpression":"#i","ExpressionAttributeNames":{"#i":"id"},`+
		`"ConsistentRead":true}}}`)

	got, ok := path(t, out, "Responses", "tab1").([]any)
	require.True(t, ok)
	require.Len(t, got, 43)

	rest, ok := path(t, out, "UnprocessedKeys", "tab1", "Keys").([]any)
	require.True(t, ok)
	require.Len(t, rest, 7)
	require.Equal(t, "big43", path(t, rest, 0, "id", "S"))
	require.Equal(t, "#i", path(t, out, "UnprocessedKeys", "tab1", "ProjectionExpression"))
	require.Equal(t, true, path(t, out, "UnprocessedKeys", "tab1", "ConsistentRead"))
	require.InDelta(t, 4128.0, path(t, out, "ConsumedCapacity", 0, "CapacityUnits"), 0)
}
