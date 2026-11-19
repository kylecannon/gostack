package dynamodb_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	opUpdateItem = "UpdateItem"
	opDeleteItem = "DeleteItem"
	codeCondFail = "ConditionalCheckFailedException"
	msgCondFail  = "The conditional request failed"
)

// Test_writes_conditions: a condition that fails refuses the write, with the
// stored item only when ReturnValuesOnConditionCheckFailure asks for it and
// there is one, as test_return_values_on_conditions_check_failure pins.
func Test_writes_conditions(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("tab1"))
	s.ok(t, opPut, `{"TableName":"tab1","Item":{"id":{"S":"456"},"price":{"N":"650"}}}`)

	got := s.call(t, usEast1, opDeleteItem, `{"TableName":"tab1","Key":{"id":{"S":"456"}},`+
		`"ConditionExpression":"price between :lo and :hi",`+
		`"ExpressionAttributeValues":{":lo":{"N":"500"},":hi":{"N":"600"}},`+
		`"ReturnValuesOnConditionCheckFailure":"ALL_OLD"}`)
	require.Equal(t, 400, got.status)
	require.JSONEq(t, `{"__type":"ConditionalCheckFailedException","message":"The conditional request failed",`+
		`"Item":{"id":{"S":"456"},"price":{"N":"650"}}}`, got.body)

	s.fails(t, opPut, `{"TableName":"tab1","Item":{"id":{"S":"new"}},"ConditionExpression":"attribute_exists(id)",`+
		`"ReturnValuesOnConditionCheckFailure":"ALL_OLD"}`, codeCondFail, msgCondFail)
	s.fails(t, opGet, `{"TableName":"tab1","Key":{"id":{"S":"new"}},"ExpressionAttributeNames":{"#a":"a"}}`,
		codeValidation, "ExpressionAttributeNames can only be specified when using expressions")
	s.fails(t, opPut, `{"TableName":"tab1","Item":{"id":{"S":"x"}},"ExpressionAttributeValues":{":x":{"S":"x"}}}`,
		codeValidation, "ExpressionAttributeValues can only be specified when using expressions: ConditionExpression is null")
	s.fails(t, opPut, `{"TableName":"tab1","Item":{"id":{"S":"x"}},"ReturnValuesOnConditionCheckFailure":"FOO"}`,
		codeValidation, "1 validation error detected: Value 'FOO' at 'returnValuesOnConditionCheckFailure' "+
			"failed to satisfy constraint: Member must satisfy enum value set: [ALL_OLD, NONE]")
	s.fails(t, opPut, `{"TableName":"tab1","Item":{"id":{"S":"x"}},"ReturnValues":"ALL_NEW"}`,
		codeValidation, "Return values set to invalid value")

	put := s.ok(t, opPut, `{"TableName":"tab1","Item":{"id":{"S":"456"},"price":{"N":"1"}},`+
		`"ConditionExpression":"price = :p","ExpressionAttributeValues":{":p":{"N":"650"}},"ReturnValues":"ALL_OLD"}`)
	require.Equal(t, "650", path(t, put, "Attributes", "price", "N"))

	got = s.call(t, usEast1, opGet, `{"TableName":"tab1","Key":{"id":{"S":"456"}},"ProjectionExpression":"#p",`+
		`"ExpressionAttributeNames":{"#p":"price"}}`)
	require.JSONEq(t, `{"Item":{"price":{"N":"1"}}}`, got.body)
}

// Test_writes_updateItem: UpdateItem creates the item from its key, applies
// the expression, reports the return values, and refuses a key attribute.
func Test_writes_updateItem(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("tab1"))

	created := s.ok(t, opUpdateItem, `{"TableName":"tab1","Key":{"id":{"S":"a"}},`+
		`"UpdateExpression":"SET attr1 = :v1, attr2 = :v2",`+
		`"ExpressionAttributeValues":{":v1":{"S":"value1"},":v2":{"S":"value2"}},"ReturnValues":"ALL_NEW"}`)
	require.Equal(t, map[string]any{
		"id": map[string]any{"S": "a"}, "attr1": map[string]any{"S": "value1"}, "attr2": map[string]any{"S": "value2"},
	}, path(t, created, "Attributes"))

	updated := s.ok(t, opUpdateItem, `{"TableName":"tab1","Key":{"id":{"S":"a"}},"UpdateExpression":"SET attr1 = :v",`+
		`"ExpressionAttributeValues":{":v":{"S":"x"}},"ReturnValues":"UPDATED_OLD"}`)
	require.Equal(t, map[string]any{"attr1": map[string]any{"S": "value1"}}, path(t, updated, "Attributes"))

	bare := s.ok(t, opUpdateItem, `{"TableName":"tab1","Key":{"id":{"S":"b"}},"ReturnValues":"ALL_NEW"}`)
	require.Equal(t, map[string]any{"id": map[string]any{"S": "b"}}, path(t, bare, "Attributes"))

	s.fails(t, opUpdateItem, `{"TableName":"tab1","Key":{"id":{"S":"a"}},"UpdateExpression":"SET #k = :v",`+
		`"ExpressionAttributeNames":{"#k":"id"},"ExpressionAttributeValues":{":v":{"S":"x"}}}`, codeValidation,
		"One or more parameter values were invalid: Cannot update attribute id. This attribute is part of the key")
	s.fails(t, opUpdateItem, `{"TableName":"tab1","Key":{"id":{"S":"a"}},"UpdateExpression":"SET q = :v",`+
		`"ConditionExpression":"attr1 = :v","ExpressionAttributeValues":{":v":{"S":"nope"}}}`, codeCondFail, msgCondFail)
	s.fails(t, opUpdateItem, `{"TableName":"tab1","Key":{"id":{"S":"a"}},"ExpressionAttributeValues":{":v":{"S":"x"}}}`,
		codeValidation, "ExpressionAttributeValues can only be specified when using expressions: "+
			"UpdateExpression and ConditionExpression are null")
	s.fails(t, opUpdateItem, `{"TableName":"tab1","Key":{"id":{"S":"a"}},"ReturnValues":"FOO"}`,
		codeValidation, "Return values set to invalid value")
}

// Test_writes_limits: an UpdateExpression over 4 KB is refused before it is
// parsed, and a value nested more than 32 levels deep is refused wherever it
// appears.
func Test_writes_limits(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("tab1"))

	long := "SET a = :v" + spaces(5000)
	s.fails(t, opUpdateItem, `{"TableName":"tab1","Key":{"id":{"S":"a"}},"UpdateExpression":"`+long+`",`+
		`"ExpressionAttributeValues":{":v":{"S":"x"}}}`, codeValidation,
		"Invalid UpdateExpression: Expression size has exceeded the maximum allowed size;")

	const tooDeep = "Nesting Levels have exceeded supported limits: " +
		"Attributes in the item have nested levels beyond supported limit"

	s.ok(t, opPut, `{"TableName":"tab1","Item":{"id":{"S":"a"},"d":`+nested(31)+`}}`)
	s.fails(t, opPut, `{"TableName":"tab1","Item":{"id":{"S":"a"},"d":`+nested(32)+`}}`, codeValidation, tooDeep)
	s.fails(t, opUpdateItem, `{"TableName":"tab1","Key":{"id":{"S":"a"}},"UpdateExpression":"SET q = :v",`+
		`"ExpressionAttributeValues":{":v":`+nested(40)+`}}`, codeValidation,
		"ExpressionAttributeValues contains invalid value: "+tooDeep+" for key :v")

	// 4,900 levels, about the deepest the shared JSON decoder passes on (it
	// refuses a 10,000-level item itself, past encoding/json's 10,000 nesting
	// limit): refused by the 32-level check before anything in the DynamoDB
	// packages walks the value. value's Test_Validate_deepBounded proves that
	// check's stack is bounded.
	const levels = 4900

	deep := strings.Repeat(`{"M":{"a":`, levels) + `{"S":"x"}` + strings.Repeat(`}}`, levels)
	s.fails(t, opPut, `{"TableName":"tab1","Item":{"id":{"S":"a"},"d":`+deep+`}}`, codeValidation, tooDeep)
	s.fails(t, opUpdateItem, `{"TableName":"tab1","Key":{"id":{"S":"a"}},"UpdateExpression":"SET q = :v",`+
		`"ExpressionAttributeValues":{":v":`+deep+`}}`, codeValidation,
		"ExpressionAttributeValues contains invalid value: "+tooDeep+" for key :v")
}

func spaces(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = ' '
	}

	return string(b)
}

// nested is a string value inside n maps.
func nested(n int) string {
	v := `{"S":"x"}`
	for range n {
		v = `{"M":{"a":` + v + `}}`
	}

	return v
}
