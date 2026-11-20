package dynamodb_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// atgTable is a table keyed by id and sk with a keys-only global index gkeys
// on g, a global index ginc on g that also holds x, and an item carrying an
// attribute whose name has a dot.
func atgTable(tb testing.TB, s *stack) {
	tb.Helper()

	const throughput = `"ProvisionedThroughput":{"ReadCapacityUnits":5,"WriteCapacityUnits":5}`

	s.ok(tb, opCreate, `{"TableName":"atg","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},`+
		`{"AttributeName":"sk","KeyType":"RANGE"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"},`+
		`{"AttributeName":"sk","AttributeType":"S"},{"AttributeName":"g","AttributeType":"S"}],`+
		`"GlobalSecondaryIndexes":[{"IndexName":"gkeys","KeySchema":[{"AttributeName":"g","KeyType":"HASH"}],`+
		`"Projection":{"ProjectionType":"KEYS_ONLY"},`+throughput+`},`+
		`{"IndexName":"ginc","KeySchema":[{"AttributeName":"g","KeyType":"HASH"}],`+
		`"Projection":{"ProjectionType":"INCLUDE","NonKeyAttributes":["x"]},`+throughput+`}],`+throughput+`}`)
	s.ok(tb, opPut, `{"TableName":"atg","Item":{"id":{"S":"1"},"sk":{"S":"a"},"x":{"S":"X"},"y":{"N":"1"},`+
		`"b.c":{"S":"dot"},"m":{"M":{"n":{"S":"v"}}},"g":{"S":"G"}}}`)
}

// Test_attributesToGet_reads: GetItem, BatchGetItem and Scan project by the
// legacy AttributesToGet, each name a whole top-level attribute taken as
// written, as the LocalStack image answers (verification/SQ-302 q1, q4).
func Test_attributesToGet_reads(t *testing.T) {
	s := newStack(t)
	atgTable(t, s)

	const (
		key   = `"Key":{"id":{"S":"1"},"sk":{"S":"a"}}`
		whole = `{"b.c":{"S":"dot"},"g":{"S":"G"},"id":{"S":"1"},"m":{"M":{"n":{"S":"v"}}},"sk":{"S":"a"},` +
			`"x":{"S":"X"},"y":{"N":"1"}}`
	)

	for _, c := range []struct{ name, op, body, want string }{
		{
			"get", opGet, `{"TableName":"atg",` + key + `,"AttributesToGet":["x","b.c","m.n","zz"]}`,
			`{"Item":{"b.c":{"S":"dot"},"x":{"S":"X"}}}`,
		},
		{"get empty list", opGet, `{"TableName":"atg",` + key + `,"AttributesToGet":[]}`, `{"Item":` + whole + `}`},
		{"get none there", opGet, `{"TableName":"atg",` + key + `,"AttributesToGet":["zz"]}`, `{"Item":{}}`},
		{
			"get capacity", opGet, `{"TableName":"atg",` + key + `,"AttributesToGet":["x"],"ReturnConsumedCapacity":"TOTAL"}`,
			`{"Item":{"x":{"S":"X"}},"ConsumedCapacity":{"CapacityUnits":0.5,"TableName":"atg"}}`,
		},
		{
			"batch", opBatchGet, `{"RequestItems":{"atg":{"Keys":[{"id":{"S":"1"},"sk":{"S":"a"}}],` +
				`"AttributesToGet":["x","b.c"]}}}`,
			`{"Responses":{"atg":[{"b.c":{"S":"dot"},"x":{"S":"X"}}]},"UnprocessedKeys":{}}`,
		},
		{
			"scan", opScan, `{"TableName":"atg","AttributesToGet":["x","zz"]}`,
			`{"Count":1,"ScannedCount":1,"Items":[{"x":{"S":"X"}}]}`,
		},
		{
			"scan specific", opScan, `{"TableName":"atg","AttributesToGet":["x"],"Select":"SPECIFIC_ATTRIBUTES"}`,
			`{"Count":1,"ScannedCount":1,"Items":[{"x":{"S":"X"}}]}`,
		},
		{
			"scan index", opScan, `{"TableName":"atg","IndexName":"ginc","AttributesToGet":["x","g","sk"]}`,
			`{"Count":1,"ScannedCount":1,"Items":[{"g":{"S":"G"},"sk":{"S":"a"},"x":{"S":"X"}}]}`,
		},
	} {
		got := s.call(t, usEast1, c.op, c.body)
		require.Equal(t, http.StatusOK, got.status, "%s: %s", c.name, got.body)
		require.JSONEq(t, c.want, got.body, c.name)
	}
}

// Test_attributesToGet_refusals: the image's refusals of AttributesToGet, in
// its order: with an expression parameter, with names, an empty name, a name
// twice, a Select it contradicts, and a global index that does not hold it.
func Test_attributesToGet_refusals(t *testing.T) {
	s := newStack(t)
	atgTable(t, s)

	const (
		key                  = `"Key":{"id":{"S":"1"},"sk":{"S":"a"}}`
		msgNamesNoExpression = "ExpressionAttributeNames can only be specified when using expressions"
		mixed                = "Can not use both expression and non-expression parameters in the same request: " +
			"Non-expression parameters: {AttributesToGet} Expression parameters: "
	)

	for _, c := range []struct{ name, op, body, code, message string }{
		{
			"get with projection on no table", opGet,
			`{"TableName":"nope",` + key + `,"AttributesToGet":["x"],"ProjectionExpression":"y"}`,
			codeValidation, mixed + "{ProjectionExpression}",
		},
		{
			"get twice on no table", opGet, `{"TableName":"nope",` + key + `,"AttributesToGet":["x","x"]}`,
			codeValidation, "Cannot have two attributes with the same name",
		},
		{
			"get empty before twice", opGet, `{"TableName":"atg",` + key + `,"AttributesToGet":["","x","x"]}`,
			codeValidation, "Empty attribute name",
		},
		{
			"get with names", opGet, `{"TableName":"atg",` + key + `,"AttributesToGet":["x"],` +
				`"ExpressionAttributeNames":{"#a":"y"}}`,
			codeValidation, msgNamesNoExpression,
		},
		{
			"batch twice on no table", opBatchGet, `{"RequestItems":{"nope":{"Keys":[{"id":{"S":"1"},"sk":{"S":"a"}}],` +
				`"AttributesToGet":["x","x"]}}}`,
			codeValidation, "One or more parameter values were invalid: Duplicate value in attribute name: x",
		},
		{
			"batch with projection", opBatchGet, `{"RequestItems":{"atg":{"Keys":[{"id":{"S":"1"}}],` +
				`"AttributesToGet":["x"],"ProjectionExpression":"y"}}}`,
			codeValidation, mixed + "{ProjectionExpression}",
		},
		{
			"query with three", opQuery, `{"TableName":"atg","KeyConditionExpression":"id = :i","FilterExpression":"x = :x",` +
				`"ProjectionExpression":"y","ExpressionAttributeValues":{":i":{"S":"1"},":x":{"S":"X"}},"AttributesToGet":["x"]}`,
			codeValidation, mixed + "{ProjectionExpression, FilterExpression, KeyConditionExpression}",
		},
		{
			"query alone", opQuery, `{"TableName":"atg","AttributesToGet":["x"]}`, codeValidation,
			"Either the KeyConditions or KeyConditionExpression parameter must be specified in the request.",
		},
		{
			"query on no table", opQuery, `{"TableName":"nope","KeyConditionExpression":"id = :i",` +
				`"ExpressionAttributeValues":{":i":{"S":"1"}},"AttributesToGet":["x"]}`,
			"ResourceNotFoundException", "Cannot do operations on a non-existent table",
		},
		{"scan with filter", opScan, `{"TableName":"atg","AttributesToGet":["x"],"FilterExpression":"x = :x",` +
			`"ExpressionAttributeValues":{":x":{"S":"X"}}}`, codeValidation, mixed + "{FilterExpression}"},
		{
			"scan select all", opScan, `{"TableName":"atg","AttributesToGet":["x"],"Select":"ALL_ATTRIBUTES"}`,
			codeValidation, "Cannot specify the AttributesToGet when choosing to get ALL_ATTRIBUTES",
		},
		{
			"scan select count before twice", opScan, `{"TableName":"atg","AttributesToGet":["x","x"],"Select":"COUNT"}`,
			codeValidation, "Cannot specify the AttributesToGet when choosing to get only the Count",
		},
		{
			"scan twice", opScan, `{"TableName":"atg","AttributesToGet":["x","x",""]}`,
			codeValidation, "Cannot have two attributes with the same name",
		},
		{
			"scan values", opScan, `{"TableName":"atg","AttributesToGet":["x"],"ExpressionAttributeValues":{":a":{"S":"y"}}}`,
			codeValidation, "ExpressionAttributeValues can only be specified when using expressions: FilterExpression is null",
		},
		{
			"scan index not holding", opScan, `{"TableName":"atg","IndexName":"ginc","AttributesToGet":["y","x","zz","id"]}`,
			codeValidation, "One or more parameter values were invalid: Global secondary index ginc does not project [zz, y]",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			s.fails(t, c.op, c.body, c.code, c.message)
		})
	}
}
