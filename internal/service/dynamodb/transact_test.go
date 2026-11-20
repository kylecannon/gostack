package dynamodb_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	opTransactWrite = "TransactWriteItems"
	opTransactGet   = "TransactGetItems"
	codeCanceled    = "TransactionCanceledException"
)

// canceled sends a call that must fail with a TransactionCanceledException
// whose message, which that error spells Message, is message.
func (s *stack) canceled(tb testing.TB, operation, body, message string) {
	tb.Helper()

	got := s.call(tb, usEast1, operation, body)
	require.Equal(tb, http.StatusBadRequest, got.status, got.body)

	var out map[string]any
	require.NoError(tb, json.Unmarshal([]byte(got.body), &out))
	require.Equal(tb, codeCanceled, out["__type"], got.body)
	require.Equal(tb, message, out["Message"], got.body)
}

// Test_transactWrite_rows: the bodies test_transaction_write_items,
// test_transaction_write_binary_data and test_transact_get_items pin, by name
// and by ARN, and what the writes did.
func Test_transactWrite_rows(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("tab1"))

	for _, name := range []string{tab1, "arn:aws:dynamodb:us-east-1:000000000000:table/tab1"} {
		got := s.call(t, usEast1, opTransactWrite, `{"TransactItems":[`+
			`{"ConditionCheck":{"TableName":"`+name+`","ConditionExpression":"attribute_not_exists(id)",`+
			`"Key":{"id":{"S":"test1"}}}},`+
			`{"Put":{"TableName":"`+name+`","Item":{"id":{"S":"test2"},"binaryData":{"B":"Zm9vYmFy"}}}},`+
			`{"Update":{"TableName":"`+name+`","Key":{"id":{"S":"test3"}},"UpdateExpression":"SET attr1 = :v1, attr2 = :v2",`+
			`"ExpressionAttributeValues":{":v1":{"S":"value1"},":v2":{"S":"value2"}}}},`+
			`{"Delete":{"TableName":"`+name+`","Key":{"id":{"S":"test4"}}}}]}`)
		require.Equal(t, http.StatusOK, got.status, got.body)
		require.JSONEq(t, `{}`, got.body)
	}

	got := s.call(t, usEast1, opTransactGet, `{"TransactItems":[`+
		`{"Get":{"TableName":"tab1","Key":{"id":{"S":"test2"}}}},`+
		`{"Get":{"TableName":"arn:aws:dynamodb:us-east-1:000000000000:table/tab1","Key":{"id":{"S":"test3"}},`+
		`"ProjectionExpression":"attr1"}},`+
		`{"Get":{"TableName":"tab1","Key":{"id":{"S":"test4"}}}}]}`)
	require.Equal(t, http.StatusOK, got.status, got.body)
	require.JSONEq(t, `{"Responses":[{"Item":{"id":{"S":"test2"},"binaryData":{"B":"Zm9vYmFy"}}},`+
		`{"Item":{"attr1":{"S":"value1"}}},{}]}`, got.body)
}

// Test_transactWrite_canceled: the error test_transaction_write_canceled
// pins, and a failed condition in action 3 of 5 leaving all five items as they
// were, with every condition evaluated and the stored item returned where
// ALL_OLD asks.
func Test_transactWrite_canceled(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("tab1"))
	s.ok(t, opPut, `{"TableName":"tab1","Item":{"id":{"S":"Fred"}}}`)

	got := s.call(t, usEast1, opTransactWrite, `{"TransactItems":[`+
		`{"ConditionCheck":{"TableName":"tab1","ConditionExpression":"attribute_not_exists(id)","Key":{"id":{"S":"Fred"}}}},`+
		`{"Delete":{"TableName":"tab1","Key":{"id":{"S":"Bert"}}}}]}`)
	require.Equal(t, http.StatusBadRequest, got.status)
	require.JSONEq(t, `{"__type":"TransactionCanceledException","Message":"Transaction cancelled, please refer `+
		`cancellation reasons for specific reasons [ConditionalCheckFailed, None]","CancellationReasons":[`+
		`{"Code":"ConditionalCheckFailed","Message":"The conditional request failed."},{"Code":"None"}]}`, got.body)

	for i := 1; i <= 5; i++ {
		s.ok(t, opPut, fmt.Sprintf(`{"TableName":"tab1","Item":{"id":{"S":"i%d"},"v":{"N":"%d"}}}`, i, i))
	}

	got = s.call(t, usEast1, opTransactWrite, `{"TransactItems":[`+
		`{"Put":{"TableName":"tab1","Item":{"id":{"S":"i1"},"v":{"N":"0"}}}},`+
		`{"Delete":{"TableName":"tab1","Key":{"id":{"S":"i2"}}}},`+
		`{"ConditionCheck":{"TableName":"tab1","Key":{"id":{"S":"i3"}},"ConditionExpression":"v = :v",`+
		`"ExpressionAttributeValues":{":v":{"N":"6"}},"ReturnValuesOnConditionCheckFailure":"ALL_OLD"}},`+
		`{"Update":{"TableName":"tab1","Key":{"id":{"S":"i4"}},"UpdateExpression":"SET v = :z",`+
		`"ExpressionAttributeValues":{":z":{"N":"0"}}}},`+
		`{"Put":{"TableName":"tab1","Item":{"id":{"S":"i5"}},"ConditionExpression":"attribute_not_exists(id)",`+
		`"ReturnValuesOnConditionCheckFailure":"NONE"}}]}`)
	require.JSONEq(t, `{"__type":"TransactionCanceledException","Message":"Transaction cancelled, please refer `+
		`cancellation reasons for specific reasons [None, None, ConditionalCheckFailed, None, ConditionalCheckFailed]",`+
		`"CancellationReasons":[{"Code":"None"},{"Code":"None"},{"Code":"ConditionalCheckFailed",`+
		`"Message":"The conditional request failed.","Item":{"id":{"S":"i3"},"v":{"N":"3"}}},{"Code":"None"},`+
		`{"Code":"ConditionalCheckFailed","Message":"The conditional request failed"}]}`, got.body)

	for i := 1; i <= 5; i++ {
		item := s.ok(t, opGet, fmt.Sprintf(`{"TableName":"tab1","Key":{"id":{"S":"i%d"}}}`, i))
		require.Equal(t, strconv.Itoa(i), path(t, item, "Item", "v", "N"), "item i%d", i)
	}
}

// Test_transactWrite_reasons: a key of the wrong type is its action's
// ValidationError, beside the other actions' reasons; an update of a key
// attribute cancels with that one reason; an update that cannot apply, or
// makes an item too large, refuses the whole request.
func Test_transactWrite_reasons(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("tab1"))
	s.ok(t, opPut, `{"TableName":"tab1","Item":{"id":{"S":"1"},"a":{"N":"5"}}}`)
	s.ok(t, opPut, `{"TableName":"tab1","Item":{"id":{"S":"2"},"a":{"S":"s"}}}`)

	failCheck := `{"ConditionCheck":{"TableName":"tab1","Key":{"id":{"S":"1"}},` +
		`"ConditionExpression":"attribute_not_exists(id)"}}`
	badType := `{"Update":{"TableName":"tab1","Key":{"id":{"S":"2"}},"UpdateExpression":"SET a = a + :one",` +
		`"ExpressionAttributeValues":{":one":{"N":"1"}}}}`

	got := s.call(t, usEast1, opTransactWrite, `{"TransactItems":[{"Put":{"TableName":"tab1","Item":{"id":{"N":"1"}}}},`+
		failCheck+`]}`)
	require.JSONEq(t, `{"__type":"TransactionCanceledException","Message":"Transaction cancelled, please refer `+
		`cancellation reasons for specific reasons [ValidationError, ConditionalCheckFailed]","CancellationReasons":[`+
		`{"Code":"ValidationError","Message":"`+msgKeyType+`"},`+
		`{"Code":"ConditionalCheckFailed","Message":"The conditional request failed."}]}`, got.body)

	got = s.call(t, usEast1, opTransactWrite, `{"TransactItems":[`+failCheck+`,{"Update":{"TableName":"tab1",`+
		`"Key":{"id":{"S":"2"}},"UpdateExpression":"SET id = :v","ExpressionAttributeValues":{":v":{"S":"z"}}}}]}`)
	require.JSONEq(t, `{"__type":"TransactionCanceledException","Message":"Transaction cancelled, please refer `+
		`cancellation reasons for specific reasons [ValidationError]","CancellationReasons":[{"Code":"ValidationError",`+
		`"Message":"One or more parameter values were invalid: Cannot update attribute id. `+
		`This attribute is part of the key"}]}`, got.body)

	s.fails(t, opTransactWrite, `{"TransactItems":[`+failCheck+`,`+badType+`]}`, codeValidation,
		"An operand in the update expression has an incorrect data type")
	s.fails(t, opTransactWrite, `{"TransactItems":[`+failCheck+`,{"Update":{"TableName":"tab1",`+
		`"Key":{"id":{"S":"3"}},"UpdateExpression":"SET d = :d","ExpressionAttributeValues":{":d":{"S":"`+
		strings.Repeat("x", 410000)+`"}}}}]}`, codeValidation, "Item size to update has exceeded the maximum allowed size")

	// An update whose condition fails is not applied, so cannot refuse.
	got = s.call(t, usEast1, opTransactWrite, `{"TransactItems":[{"Update":{"TableName":"tab1","Key":{"id":{"S":"2"}},`+
		`"ConditionExpression":"attribute_not_exists(id)","UpdateExpression":"SET a = a + :one",`+
		`"ExpressionAttributeValues":{":one":{"N":"1"}}}}]}`)
	require.Contains(t, got.body, `"CancellationReasons":[{"Code":"ConditionalCheckFailed",`+
		`"Message":"The conditional request failed"}]`)
}

// Test_transactWrite_refusals: each message DynamoDB Local answers before it
// evaluates any action, probed on the LocalStack image, in its order of
// checks.
func Test_transactWrite_refusals(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("tab1"))

	put := func(id string) string {
		return `{"Put":{"TableName":"tab1","Item":{"id":{"S":"` + id + `"}}}}`
	}
	puts := func(n int) string {
		list := make([]string, n)
		for i := range list {
			list[i] = put(strconv.Itoa(i))
		}

		return strings.Join(list, ",")
	}
	big := strings.Repeat("x", 390000)
	bigPuts := make([]string, 11)

	for i := range bigPuts {
		bigPuts[i] = fmt.Sprintf(`{"Put":{"TableName":"tab1","Item":{"id":{"S":"m%d"},"d":{"S":"%s"}}}}`, i, big)
	}

	for _, c := range []struct {
		name, body, code, message string
	}{
		{
			"no items", `{}`, codeValidation,
			msgNullItems,
		},
		{
			"empty", `{"TransactItems":[]}`, codeValidation,
			"Value [] at 'transactItems' failed to satisfy constraint: Member must have length greater than or equal to 1",
		},
		{
			"101", `{"TransactItems":[` + puts(101) + `]}`, codeValidation,
			"Member must have length less than or equal to 100",
		},
		{
			"long token", `{"ClientRequestToken":"` + strings.Repeat("x", 37) + `","TransactItems":[` + puts(1) + `]}`,
			codeValidation, "Value " + strings.Repeat("x", 37) + " at 'clientRequestToken' failed to satisfy " +
				"constraint: Member must have length less than or equal to 36",
		},
		{
			"no action", `{"TransactItems":[{}]}`, codeValidation,
			"Invalid Request: TransactWriteRequest should contain Delete or Put or Update request",
		},
		{
			"two actions after a bad name", `{"TransactItems":[{"Put":{"TableName":"a","Item":{"id":{"S":"1"}}}},` +
				`{"Put":{"TableName":"tab1","Item":{"id":{"S":"1"}}},"Delete":{"TableName":"tab1","Key":{"id":{"S":"2"}}}}]}`,
			codeValidation, "TransactItems can only contain one of Check, Put, Update or Delete",
		},
		{
			"4 MB, before a bad key", `{"TransactItems":[{"Put":{"TableName":"tab1","Item":{"x":{"S":"1"}}}},` +
				strings.Join(bigPuts, ",") + `]}`, codeValidation,
			"Transaction payload size cannot exceed 4MB. Payload Size: 4290058",
		},
		{
			"empty item on a missing table", `{"TransactItems":[{"Put":{"TableName":"non-existing-table","Item":{}}}]}`,
			codeNotFound, msgNoTable,
		},
		{
			"no table name", `{"TransactItems":[{"Put":{"Item":{"id":{"S":"1"}}}}]}`, codeValidation,
			"Value null at 'transactItems.1.member.put.tableName' failed to satisfy constraint: Member must not be null",
		},
		{
			"no item", `{"TransactItems":[{"Put":{"TableName":"tab1"}}]}`, codeValidation,
			"Value null at 'transactItems.1.member.put.item' failed to satisfy constraint: Member must not be null",
		},
		{"no condition", `{"TransactItems":[` + put("a") + `,{"ConditionCheck":{"TableName":"tab1",` +
			`"Key":{"id":{"S":"1"}}}}]}`, codeValidation, "Value null at 'transactItems.2.member.conditionCheck." +
			"conditionExpression' failed to satisfy constraint: Member must not be null"},
		{
			caseMissingKey, `{"TransactItems":[{"Put":{"TableName":"tab1","Item":{"x":{"S":"1"}}}}]}`, codeValidation,
			msgKeyNotGiven,
		},
		{
			caseExtraKey, `{"TransactItems":[{"Delete":{"TableName":"tab1","Key":{"id":{"S":"1"},"a":{"S":"1"}}}}]}`,
			codeValidation, msgKeyCount,
		},
		{
			"empty key", `{"TransactItems":[{"Put":{"TableName":"tab1","Item":{"id":{"S":""}}}}]}`, codeValidation,
			"One or more parameter values are not valid. The AttributeValue for a key attribute cannot contain an " +
				"empty string value. Key: id",
		},
		{
			"bad condition before a missing table", `{"TransactItems":[{"Put":{"TableName":"tab1",` +
				`"Item":{"id":{"S":"1"}},"ConditionExpression":"a = "}},{"Put":{"TableName":"nope","Item":{"id":{"S":"1"}}}}]}`,
			codeValidation, `Invalid ConditionExpression: Syntax error; token: "<EOF>", near: "= "`,
		},
		{
			"unused value", `{"TransactItems":[{"Put":{"TableName":"tab1","Item":{"id":{"S":"1"}},` +
				`"ConditionExpression":"a = :a","ExpressionAttributeValues":{":a":{"S":"1"},":b":{"S":"1"}}}}]}`,
			codeValidation, "Value provided in ExpressionAttributeValues unused in expressions: keys: {:b}",
		},
		{
			"one item twice, once by ARN", `{"TransactItems":[` + put("1") + `,{"Delete":{"TableName":` +
				`"arn:aws:dynamodb:us-east-1:000000000000:table/tab1","Key":{"id":{"S":"1"}}}}]}`, codeValidation,
			"Transaction request cannot include multiple operations on one item",
		},
		{"bad key before a duplicate", `{"TransactItems":[{"Put":{"TableName":"tab1","Item":{"x":{"S":"1"}}}},` +
			put("1") + `,` + put("1") + `]}`, codeValidation, msgKeyNotGiven},
	} {
		t.Run(c.name, func(t *testing.T) {
			s.fails(t, opTransactWrite, c.body, c.code, c.message)
		})
	}

	s.ok(t, opTransactWrite, `{"TransactItems":[`+puts(100)+`]}`)
}

// Test_transactWrite_capacity: transactional units per table in DynamoDB
// Local's table order, summed per action.
func Test_transactWrite_capacity(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("tw1"))
	s.ok(t, opCreate, rangeKeyTable("tw2"))
	s.ok(t, opPut, `{"TableName":"tw1","Item":{"id":{"S":"1"},"d":{"S":"`+strings.Repeat("x", 5000)+`"}}}`)

	got := s.call(t, usEast1, opTransactWrite, `{"ReturnConsumedCapacity":"TOTAL","TransactItems":[`+
		`{"Put":{"TableName":"tw1","Item":{"id":{"S":"11"}}}},`+
		`{"ConditionCheck":{"TableName":"tw1","Key":{"id":{"S":"1"}},"ConditionExpression":"attribute_exists(id)"}},`+
		`{"Delete":{"TableName":"tw1","Key":{"id":{"S":"none"}}}},`+
		`{"Put":{"TableName":"tw2","Item":{"id":{"S":"1"},"sk":{"S":"1"}}}}]}`)
	require.JSONEq(t, `{"ConsumedCapacity":[{"TableName":"tw2","CapacityUnits":2},`+
		`{"TableName":"tw1","CapacityUnits":7}]}`, got.body)

	got = s.call(t, usEast1, opTransactGet, `{"ReturnConsumedCapacity":"INDEXES","TransactItems":[`+
		`{"Get":{"TableName":"tw1","Key":{"id":{"S":"1"}}}},{"Get":{"TableName":"tw1","Key":{"id":{"S":"none"}}}}]}`)
	require.Contains(t, got.body, `"ConsumedCapacity":[{"TableName":"tw1","CapacityUnits":6,"Table":{"CapacityUnits":6}}]`)
}

// Test_transactGet_refusals: TransactGetItems' own messages, and a key the
// table refuses answered as a TransactionCanceledException with no reasons.
func Test_transactGet_refusals(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("tab1"))

	get := `{"Get":{"TableName":"tab1","Key":{"id":{"S":"1"}}}}`

	for _, c := range []struct {
		name, body, code, message string
	}{
		{
			"empty", `{"TransactItems":[]}`, codeValidation,
			msgNullItems,
		},
		{
			"no get", `{"TransactItems":[{}]}`, codeValidation,
			msgNullItems,
		},
		{
			"101", `{"TransactItems":[` + strings.TrimSuffix(strings.Repeat(get+",", 101), ",") + `]}`, codeValidation,
			"Member must have length less than or equal to 100",
		},
		{
			caseMissingTable, `{"TransactItems":[{"Get":{"TableName":"nope","Key":{"id":{"S":"1"}}}}]}`, codeNotFound,
			msgNoTable,
		},
		{
			"no key", `{"TransactItems":[{"Get":{"TableName":"tab1"}}]}`, codeValidation,
			"Get Condition for key not in key list",
		},
		{
			caseMissingKey, `{"TransactItems":[{"Get":{"TableName":"tab1","Key":{"x":{"S":"1"}}}}]}`, codeCanceled,
			msgKeyNotGiven,
		},
		{
			caseKeyType, `{"TransactItems":[{"Get":{"TableName":"tab1","Key":{"id":{"N":"1"}}}}]}`, codeCanceled,
			msgKeyType,
		},
		{
			"twice", `{"TransactItems":[` + get + `,` + get + `]}`, codeValidation,
			"Transaction request cannot include multiple operations on one item",
		},
		{
			"names with no projection", `{"TransactItems":[{"Get":{"TableName":"tab1","Key":{"id":{"S":"1"}},` +
				`"ExpressionAttributeNames":{"#a":"a"}}}]}`, codeValidation,
			"ExpressionAttributeNames can only be specified when using expressions",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.code == codeCanceled {
				s.canceled(t, opTransactGet, c.body, c.message)

				return
			}

			s.fails(t, opTransactGet, c.body, c.code, c.message)
		})
	}
}

// Test_transactWrite_idempotent: what test_dynamodb_idempotent_writing pins,
// the same request with its item's attributes in the other order replaying,
// and the cases around it: a replay writes nothing, another request with the
// token is refused, and a failed transaction records nothing.
func Test_transactWrite_idempotent(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("tab1"))

	write := func(item string) reply {
		return s.call(t, usEast1, opTransactWrite, `{"ClientRequestToken":"dedupe_token","TransactItems":[`+
			`{"Put":{"TableName":"tab1","Item":`+item+`}}]}`)
	}

	got := write(`{"id":{"S":"id1"},"n":{"N":"1"}}`)
	require.Equal(t, http.StatusOK, got.status, got.body)
	require.JSONEq(t, `{}`, got.body)

	s.ok(t, opPut, `{"TableName":"tab1","Item":{"id":{"S":"id1"},"n":{"N":"2"}}}`)

	got = write(`{"n":{"N":"1"},"id":{"S":"id1"}}`)
	require.Equal(t, http.StatusOK, got.status, got.body)
	require.Equal(t, "2", path(t, s.ok(t, opGet, `{"TableName":"tab1","Key":{"id":{"S":"id1"}}}`), "Item", "n", "N"))

	got = s.call(t, usEast1, opTransactWrite, `{"ClientRequestToken":"dedupe_token","ReturnConsumedCapacity":"TOTAL",`+
		`"TransactItems":[{"Put":{"TableName":"tab1","Item":{"id":{"S":"id1"},"n":{"N":"1"}}}}]}`)
	require.JSONEq(t, `{"ConsumedCapacity":[{"TableName":"tab1","CapacityUnits":2}]}`, got.body)

	for _, item := range []string{`{"id":{"S":"id1"},"n":{"N":"3"}}`, `{"id":{"S":"id1"},"n":{"N":"1.0"}}`} {
		got = write(item)
		require.Equal(t, http.StatusBadRequest, got.status)
		require.Contains(t, got.body, `"__type":"IdempotentParameterMismatchException"`)
	}

	failing := `{"ClientRequestToken":"tok2","TransactItems":[{"Put":{"TableName":"tab1","Item":{"id":{"S":"id1"}},` +
		`"ConditionExpression":"attribute_not_exists(id)"}}]}`
	s.canceled(t, opTransactWrite, failing, "Transaction cancelled, please refer cancellation reasons "+
		"for specific reasons [ConditionalCheckFailed]")
	s.canceled(t, opTransactWrite, failing, "Transaction cancelled, please refer cancellation reasons "+
		"for specific reasons [ConditionalCheckFailed]")
}
