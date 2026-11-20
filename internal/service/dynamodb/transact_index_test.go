package dynamodb_test

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// gixTable is a CreateTable body for a table keyed by id with a global index
// gix on the string g, projecting every attribute.
func gixTable(name string) string {
	const throughput = `"ProvisionedThroughput":{"ReadCapacityUnits":5,"WriteCapacityUnits":5}`

	return `{"TableName":"` + name + `","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],` +
		`"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"},{"AttributeName":"g","AttributeType":"S"}],` +
		`"GlobalSecondaryIndexes":[{"IndexName":"gix","KeySchema":[{"AttributeName":"g","KeyType":"HASH"}],` +
		`"Projection":{"ProjectionType":"ALL"},` + throughput + `}],` + throughput + `}`
}

// Test_transactWrite_indexKeyType: a put or an update that gives the index key
// a number cancels the transaction with that action's ValidationError, as the
// LocalStack image answers (verification/SQ-293 p1b), and writes nothing.
func Test_transactWrite_indexKeyType(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, gixTable("tcg"))
	s.ok(t, opPut, `{"TableName":"tcg","Item":{"id":{"S":"1"},"g":{"S":"old"}}}`)

	const want = `{"__type":"TransactionCanceledException","Message":"Transaction cancelled, please refer ` +
		`cancellation reasons for specific reasons [None, ValidationError]","CancellationReasons":[{"Code":"None"},` +
		`{"Code":"ValidationError","Message":"One or more parameter values were invalid: Type mismatch for Index Key"}]}`

	for name, action := range map[string]string{
		"put": `{"Put":{"TableName":"tcg","Item":{"id":{"S":"2"},"g":{"N":"1"}}}}`,
		"update": `{"Update":{"TableName":"tcg","Key":{"id":{"S":"1"}},"UpdateExpression":"SET g = :g",` +
			`"ExpressionAttributeValues":{":g":{"N":"1"}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			got := s.call(t, usEast1, opTransactWrite, `{"TransactItems":[`+
				`{"Put":{"TableName":"tcg","Item":{"id":{"S":"3"},"g":{"S":"x"}}}},`+action+`]}`)
			require.Equal(t, http.StatusBadRequest, got.status, got.body)
			require.JSONEq(t, want, got.body)

			scan := s.ok(t, "Scan", `{"TableName":"tcg"}`)
			require.InDelta(t, 1, scan["Count"], 0, "the transaction wrote nothing")
		})
	}
}

// Test_transactWrite_indexCapacity: a transaction's consumed capacity counts
// the index units of each write, twice a plain write's, as the LocalStack
// image answers: a put entering gix, an update moving its item in gix, and a
// delete of no item give 11 units, 5 on the table and 6 on gix.
func Test_transactWrite_indexCapacity(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, gixTable("tcg"))
	s.ok(t, opPut, `{"TableName":"tcg","Item":{"id":{"S":"1"},"g":{"S":"old"}}}`)

	got := s.call(t, usEast1, opTransactWrite, `{"ReturnConsumedCapacity":"INDEXES","TransactItems":[`+
		`{"Put":{"TableName":"tcg","Item":{"id":{"S":"4"},"g":{"S":"v"}}}},`+
		`{"Update":{"TableName":"tcg","Key":{"id":{"S":"1"}},"UpdateExpression":"SET g = :g",`+
		`"ExpressionAttributeValues":{":g":{"S":"new"}}}},`+
		`{"Delete":{"TableName":"tcg","Key":{"id":{"S":"99"}}}}]}`)
	require.Equal(t, http.StatusOK, got.status, got.body)
	require.JSONEq(t, `{"ConsumedCapacity":[{"TableName":"tcg","CapacityUnits":11,"Table":{"CapacityUnits":5},`+
		`"GlobalSecondaryIndexes":{"gix":{"CapacityUnits":6}}}]}`, got.body)

	got = s.call(t, usEast1, opTransactWrite, `{"ReturnConsumedCapacity":"TOTAL","TransactItems":[`+
		`{"Put":{"TableName":"tcg","Item":{"id":{"S":"5"},"g":{"S":"v"}}}}]}`)
	require.Equal(t, http.StatusOK, got.status, got.body)
	require.JSONEq(t, `{"ConsumedCapacity":[{"TableName":"tcg","CapacityUnits":4}]}`, got.body)
}

// Test_transactGet_responseSize: TransactGetItems refuses a response whose
// items, each counted as its size plus 4 bytes, pass 4 MiB, naming that total
// as the LocalStack image does (verification/SQ-302 q2). The same gets
// projected to the key pass, and a missing item counts nothing.
func Test_transactGet_responseSize(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("lim"))

	// Ten items of 381,300 bytes and one making the total; each is id plus d.
	put := func(total int) []string {
		keys := make([]string, 0, 11)

		for i := range 11 {
			id := "m" + strconv.Itoa(10+i)
			size := 381300
			if i == 10 {
				size = total - 10*381300
			}

			pad := strings.Repeat("x", size-(2+len(id))-1)
			s.ok(t, opPut, `{"TableName":"lim","Item":{"id":{"S":"`+id+`"},"d":{"S":"`+pad+`"}}}`)
			keys = append(keys, `{"Get":{"TableName":"lim","Key":{"id":{"S":"`+id+`"}}PROJ}}`)
		}

		return keys
	}

	gets := func(keys []string, projection string) string {
		body := `{"TransactItems":[` + strings.Join(keys, ",") + `]}`

		return strings.ReplaceAll(body, "PROJ", projection)
	}

	keys := put(4194304 - 44)
	got := s.call(t, usEast1, opTransactGet, gets(keys, ""))
	require.Equal(t, http.StatusOK, got.status, "4,194,260 bytes in 11 items is 4 MiB with its overhead")

	keys = put(4194304 - 43)
	s.fails(t, opTransactGet, gets(keys, ""), codeValidation, "Payload in response is too large.4194305")

	missing := append(slices.Clone(keys), `{"Get":{"TableName":"lim","Key":{"id":{"S":"none"}}PROJ}}`)
	s.fails(t, opTransactGet, gets(missing, ""), codeValidation, "Payload in response is too large.4194305")

	got = s.call(t, usEast1, opTransactGet, gets(keys, `,"ProjectionExpression":"id"`))
	require.Equal(t, http.StatusOK, got.status, "the projected items are what count")
}
