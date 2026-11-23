package dynamodb_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Test_reads_keysOnlyIgnoresNonKeyAttributes: an index that is KEYS_ONLY holds
// its keys whatever NonKeyAttributes it was created with, as main's
// IndexSpec.Project held it. NonKeyAttributes count for INCLUDE alone.
func Test_reads_keysOnlyIgnoresNonKeyAttributes(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, `{"TableName":"keys-only","BillingMode":"PAY_PER_REQUEST",`+
		`"KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],`+
		`"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"},{"AttributeName":"g","AttributeType":"S"}],`+
		`"GlobalSecondaryIndexes":[{"IndexName":"by-g","KeySchema":[{"AttributeName":"g","KeyType":"HASH"}],`+
		`"Projection":{"ProjectionType":"KEYS_ONLY","NonKeyAttributes":["x"]}}]}`)

	// x is wide, so an index entry that held it would cost more than 0.5.
	s.ok(t, opPut, `{"TableName":"keys-only","Item":{"id":{"S":"1"},"g":{"S":"a"},`+
		`"x":{"S":"`+strings.Repeat("x", 6000)+`"},"y":{"S":"no"}}}`)

	got := s.ok(t, opQuery, `{"TableName":"keys-only","IndexName":"by-g","KeyConditionExpression":"g = :g",`+
		`"ExpressionAttributeValues":{":g":{"S":"a"}},"ReturnConsumedCapacity":"INDEXES"}`)
	require.ElementsMatch(t, []string{"id", "g"}, keys(t, path(t, got, "Items", 0)))
	require.InDelta(t, 0.5, path(t, got, "ConsumedCapacity", "GlobalSecondaryIndexes", "by-g", "CapacityUnits"), 0)

	// The index does not hold x, so a global index refuses a projection of it.
	s.fails(t, opQuery, `{"TableName":"keys-only","IndexName":"by-g","KeyConditionExpression":"g = :g",`+
		`"ProjectionExpression":"id, x","ExpressionAttributeValues":{":g":{"S":"a"}}}`,
		"ValidationException", "One or more parameter values were invalid: Global secondary index by-g does not project [x]")
}
