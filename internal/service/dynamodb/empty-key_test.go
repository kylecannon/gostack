package dynamodb_test

import (
	"strings"
	"testing"
)

const (
	emptyStr = `One or more parameter values are not valid. ` +
		`The AttributeValue for a key attribute cannot contain an empty string value. Key: `
	emptyBin = `One or more parameter values are not valid. ` +
		`The AttributeValue for a key attribute cannot contain an empty binary value. Key: `
	emptyIdx = `One or more parameter values are not valid. A value specified for a secondary index key is not ` +
		`supported. The AttributeValue for a key attribute cannot contain an empty string value. `
	startInvalid = "The provided starting key is invalid: "
	oneRow       = "The query can return at most one row and cannot be restarted. "
)

// emptyKeyTable is a table keyed by the strings pk and sk, with a global
// index keyed by gk and gs and a local one keyed by ls.
const emptyKeyTable = `{"TableName":"ekeys","BillingMode":"PAY_PER_REQUEST",` +
	`"KeySchema":[{"AttributeName":"pk","KeyType":"HASH"},{"AttributeName":"sk","KeyType":"RANGE"}],` +
	`"AttributeDefinitions":[{"AttributeName":"pk","AttributeType":"S"},{"AttributeName":"sk","AttributeType":"S"},` +
	`{"AttributeName":"gk","AttributeType":"S"},{"AttributeName":"gs","AttributeType":"S"},` +
	`{"AttributeName":"ls","AttributeType":"S"}],` +
	`"GlobalSecondaryIndexes":[{"IndexName":"gsi1","KeySchema":[{"AttributeName":"gk","KeyType":"HASH"},` +
	`{"AttributeName":"gs","KeyType":"RANGE"}],"Projection":{"ProjectionType":"ALL"}}],` +
	`"LocalSecondaryIndexes":[{"IndexName":"lsi1","KeySchema":[{"AttributeName":"pk","KeyType":"HASH"},` +
	`{"AttributeName":"ls","KeyType":"RANGE"}],"Projection":{"ProjectionType":"ALL"}}]}`

const emptyBinTable = `{"TableName":"ebins","BillingMode":"PAY_PER_REQUEST",` +
	`"KeySchema":[{"AttributeName":"pk","KeyType":"HASH"},{"AttributeName":"sk","KeyType":"RANGE"}],` +
	`"AttributeDefinitions":[{"AttributeName":"pk","AttributeType":"B"},{"AttributeName":"sk","AttributeType":"B"}]}`

// legacy is a Query of ekeys by the legacy KeyConditions: pk is "p" and
// rangeCond the condition on sk.
func legacy(rangeCond string) string {
	return `{"TableName":"ekeys","KeyConditions":{"pk":{"ComparisonOperator":"EQ","AttributeValueList":[{"S":"p"}]},` +
		rangeCond + `}}`
}

// vs ends a request body with the ExpressionAttributeValues naming the given
// placeholders: :p is "p", :e is the empty string and :x is "x". A request
// that holds a value no expression uses is refused for that first.
func vs(names ...string) string {
	all := map[string]string{":p": `{"S":"p"}`, ":e": `{"S":""}`, ":x": `{"S":` + `"x"}`}
	parts := make([]string, len(names))

	for i, n := range names {
		parts[i] = `"` + n + `":` + all[n]
	}

	return `"ExpressionAttributeValues":{` + strings.Join(parts, ",") + "}}"
}

// Test_query_emptyKeyValues: a key condition that compares a key attribute
// with an empty string or binary is refused whatever the operator, the way a
// write of an empty key is, and an exclusive start key with an empty key
// value is refused too, a partition's empty value adding that the query
// cannot be restarted. Every row is the answer of the LocalStack image
// (SQ-280); the key attribute of a non-key attribute is no key, so a filter
// may compare with an empty string.
//
//nolint:lll // each row holds the exact text the LocalStack image answered
func Test_query_emptyKeyValues(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, emptyKeyTable)
	s.ok(t, opCreate, emptyBinTable)

	const table = `{"TableName":"ekeys",`

	for _, tt := range []struct {
		name, op, body, want string
	}{
		{"sk equal", opQuery, table + `"KeyConditionExpression":"pk = :p AND sk = :e",` + vs(":p", ":e"), emptyStr + "sk"},
		{"pk equal", opQuery, table + `"KeyConditionExpression":"pk = :e",` + vs(":e"), emptyStr + "pk"},
		{"pk then sk", opQuery, table + `"KeyConditionExpression":"pk = :e AND sk = :e",` + vs(":e"), emptyStr + "pk"},
		{"swapped operands", opQuery, table + `"KeyConditionExpression":":e = sk AND pk = :p",` + vs(":p", ":e"), emptyStr + "sk"},
		{"begins_with", opQuery, table + `"KeyConditionExpression":"pk = :p AND begins_with(sk, :e)",` + vs(":p", ":e"), emptyStr + "sk"},
		{"between low", opQuery, table + `"KeyConditionExpression":"pk = :p AND sk BETWEEN :e AND :x",` + vs(":p", ":e", ":x"), emptyStr + "sk"},
		{"between both", opQuery, table + `"KeyConditionExpression":"pk = :p AND sk BETWEEN :e AND :e",` + vs(":p", ":e"), emptyStr + "sk"},
		{"greater", opQuery, table + `"KeyConditionExpression":"pk = :p AND sk > :e",` + vs(":p", ":e"), emptyStr + "sk"},
		{"less or equal", opQuery, table + `"KeyConditionExpression":"pk = :p AND sk <= :e",` + vs(":p", ":e"), emptyStr + "sk"},
		{
			"between bounds first", opQuery, table + `"KeyConditionExpression":"pk = :p AND sk BETWEEN :x AND :e",` + vs(":p", ":x", ":e"),
			"Invalid KeyConditionExpression: The BETWEEN operator requires upper bound to be greater than or equal to " +
				"lower bound; lower bound operand: AttributeValue: {S:x}, upper bound operand: AttributeValue: {S:}",
		},
		{
			"unused value first", opQuery, table + `"KeyConditionExpression":"pk = :p AND sk = :e",` +
				`"ExpressionAttributeValues":{":p":{"S":"p"},":e":{"S":""},":u":{"S":"u"}}}`,
			"Value provided in ExpressionAttributeValues unused in expressions: keys: {:u}",
		},
		{
			"before the filter on a key", opQuery, table + `"KeyConditionExpression":"pk = :p AND sk = :e",` +
				`"FilterExpression":"sk = :x",` + vs(":p", ":e", ":x"), emptyStr + "sk",
		},
		{
			"index hash", opQuery, `{"TableName":"ekeys","IndexName":"gsi1","KeyConditionExpression":"gk = :e",` + vs(":e"),
			emptyIdx + "IndexName: gsi1, IndexKey: gk",
		},
		{
			"index range", opQuery, `{"TableName":"ekeys","IndexName":"gsi1","KeyConditionExpression":"gk = :p AND gs = :e",` + vs(":p", ":e"),
			emptyIdx + "IndexName: gsi1, IndexKey: gs",
		},
		{
			"local index range", opQuery, `{"TableName":"ekeys","IndexName":"lsi1","KeyConditionExpression":"pk = :p AND begins_with(ls, :e)",` + vs(":p", ":e"),
			emptyIdx + "IndexName: lsi1, IndexKey: ls",
		},
		{
			"local index hash", opQuery, `{"TableName":"ekeys","IndexName":"lsi1","KeyConditionExpression":"pk = :e",` + vs(":e"),
			emptyIdx + "IndexName: lsi1, IndexKey: pk",
		},
		{
			"legacy range equal", opQuery, legacy(`"sk":{"ComparisonOperator":"EQ","AttributeValueList":[{"S":""}]}`), emptyStr + "sk",
		},
		{
			"legacy hash", opQuery, `{"TableName":"ekeys","KeyConditions":{"pk":{"ComparisonOperator":"EQ",` +
				`"AttributeValueList":[{"S":""}]}}}`, emptyStr + "pk",
		},
		{
			"legacy begins with", opQuery, legacy(`"sk":{"ComparisonOperator":"BEGINS_WITH","AttributeValueList":[{"S":""}]}`),
			emptyStr + "sk",
		},
		{
			"legacy between", opQuery, legacy(`"sk":{"ComparisonOperator":"BETWEEN","AttributeValueList":[{"S":""},{"S":"x"}]}`),
			emptyStr + "sk",
		},
		{
			"legacy greater", opQuery, legacy(`"sk":{"ComparisonOperator":"GT","AttributeValueList":[{"S":""}]}`), emptyStr + "sk",
		},
		{
			"legacy index range", opQuery, `{"TableName":"ekeys","IndexName":"gsi1","KeyConditions":{"gk":{"ComparisonOperator":"EQ",` +
				`"AttributeValueList":[{"S":"p"}]},"gs":{"ComparisonOperator":"EQ","AttributeValueList":[{"S":""}]}}}`,
			emptyIdx + "IndexName: gsi1, IndexKey: gs",
		},
		{
			"legacy binary", opQuery, `{"TableName":"ebins","KeyConditions":{"pk":{"ComparisonOperator":"EQ",` +
				`"AttributeValueList":[{"B":""}]}}}`, emptyBin + "pk",
		},
		{
			"binary range", opQuery, `{"TableName":"ebins","KeyConditionExpression":"pk = :p AND sk = :e",` +
				`"ExpressionAttributeValues":{":p":{"B":"cA=="},":e":{"B":""}}}`, emptyBin + "sk",
		},
		{
			"binary hash", opQuery, `{"TableName":"ebins","KeyConditionExpression":"pk = :e",` +
				`"ExpressionAttributeValues":{":e":{"B":""}}}`, emptyBin + "pk",
		},
		{
			"start range", opQuery, table + `"KeyConditionExpression":"pk = :p",` +
				`"ExclusiveStartKey":{"pk":{"S":"p"},"sk":{"S":""}},` + vs(":p"), startInvalid + emptyStr + "sk",
		},
		{
			"start hash", opQuery, table + `"KeyConditionExpression":"pk = :p",` +
				`"ExclusiveStartKey":{"pk":{"S":""},"sk":{"S":"x"}},` + vs(":p"), oneRow + startInvalid + emptyStr + "pk",
		},
		{
			"scan start range", opScan, table + `"ExclusiveStartKey":{"pk":{"S":"p"},"sk":{"S":""}}}`,
			startInvalid + emptyStr + "sk",
		},
		{
			"scan start hash", opScan, table + `"ExclusiveStartKey":{"pk":{"S":""},"sk":{"S":"x"}}}`,
			oneRow + startInvalid + emptyStr + "pk",
		},
		{
			"scan start binary range", opScan, `{"TableName":"ebins","ExclusiveStartKey":{"pk":{"B":"cA=="},"sk":{"B":""}}}`,
			startInvalid + emptyBin + "sk",
		},
		{
			"scan start binary hash", opScan, `{"TableName":"ebins","ExclusiveStartKey":{"pk":{"B":""},"sk":{"B":"cA=="}}}`,
			oneRow + startInvalid + emptyBin + "pk",
		},
		{
			"index start range", opScan, `{"TableName":"ekeys","IndexName":"gsi1","ExclusiveStartKey":` +
				`{"pk":{"S":"p"},"sk":{"S":"x"},"gk":{"S":"p"},"gs":{"S":""}}}`,
			startInvalid + emptyIdx + "IndexName: gsi1, IndexKey: gs",
		},
		{
			"index start hash", opQuery, `{"TableName":"ekeys","IndexName":"gsi1","KeyConditionExpression":"gk = :p",` +
				`"ExclusiveStartKey":{"pk":{"S":"p"},"sk":{"S":"x"},"gk":{"S":""},"gs":{"S":"x"}},` + vs(":p"),
			oneRow + startInvalid + emptyIdx + "IndexName: gsi1, IndexKey: gk",
		},
		{
			"index start, table range first", opScan, `{"TableName":"ekeys","IndexName":"gsi1","ExclusiveStartKey":` +
				`{"pk":{"S":"p"},"sk":{"S":""},"gk":{"S":""},"gs":{"S":""}}}`,
			startInvalid + emptyStr + "sk",
		},
		{
			"local index start", opScan, `{"TableName":"ekeys","IndexName":"lsi1","ExclusiveStartKey":` +
				`{"pk":{"S":"p"},"sk":{"S":"x"},"ls":{"S":""}}}`,
			startInvalid + emptyIdx + "IndexName: lsi1, IndexKey: ls",
		},
		{
			"start before the range it must meet", opQuery, table + `"KeyConditionExpression":"pk = :p AND sk = :x",` +
				`"ExclusiveStartKey":{"pk":{"S":"p"},"sk":{"S":""}},` + vs(":p", ":x"), startInvalid + emptyStr + "sk",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s.fails(t, tt.op, tt.body, codeValidation, tt.want)
		})
	}

	// A filter compares a non-key attribute with an empty string as it likes.
	s.ok(t, opQuery, table+`"KeyConditionExpression":"pk = :p","FilterExpression":"a = :e",`+vs(":p", ":e"))
	s.ok(t, opScan, table+`"FilterExpression":"sk = :e",`+vs(":e"))
}
