//nolint:unqueryvet // the statements under test are PartiQL, SELECT * included
package dynamodb_test

import (
	"bytes"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// packedTable is a table keyed by the string pk and the number sk.
const packedTable = `{"TableName":"%s","KeySchema":[{"AttributeName":"pk","KeyType":"HASH"},` +
	`{"AttributeName":"sk","KeyType":"RANGE"}],"AttributeDefinitions":[{"AttributeName":"pk","AttributeType":"S"},` +
	`{"AttributeName":"sk","AttributeType":"N"}],"BillingMode":"PAY_PER_REQUEST"}`

// Test_packed_emptyAndCount: a read that writes its items as stored still
// writes "Items":[] when it finds none, a COUNT read writes no Items at all,
// and a BatchGetItem that finds nothing writes an empty list for the table.
func Test_packed_emptyAndCount(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, fmt.Sprintf(packedTable, "tab"))

	empty := s.call(t, usEast1, opQuery, `{"TableName":"tab","KeyConditionExpression":"pk = :h",`+
		`"ExpressionAttributeValues":{":h":{"S":"none"}}}`)
	require.JSONEq(t, `{"Items":[],"Count":0,"ScannedCount":0}`, empty.body)

	scan := s.call(t, usEast1, opScan, `{"TableName":"tab"}`)
	require.JSONEq(t, `{"Items":[],"Count":0,"ScannedCount":0}`, scan.body)

	s.ok(t, opPut, `{"TableName":"tab","Item":{"pk":{"S":"a"},"sk":{"N":"1"},"v":{"S":"x"}}}`)

	count := s.call(t, usEast1, opQuery, `{"TableName":"tab","KeyConditionExpression":"pk = :h","Select":"COUNT",`+
		`"ExpressionAttributeValues":{":h":{"S":"a"}}}`)
	require.JSONEq(t, `{"Count":1,"ScannedCount":1}`, count.body)

	batch := s.call(t, usEast1, opBatchGet, `{"RequestItems":{"tab":{"Keys":[{"pk":{"S":"b"},"sk":{"N":"1"}}]}}}`)
	require.JSONEq(t, `{"Responses":{"tab":[]},"UnprocessedKeys":{}}`, batch.body)

	missing := s.call(t, usEast1, opGet, `{"TableName":"tab","Key":{"pk":{"S":"b"},"sk":{"N":"1"}}}`)
	require.JSONEq(t, `{}`, missing.body)
}

// Test_packed_statementsReadItems: ExecuteStatement and BatchExecuteStatement
// read another handler's typed output, so they still return the items a plain
// read writes from the stored bytes.
func Test_packed_statementsReadItems(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, fmt.Sprintf(packedTable, "tab"))

	item := `{"pk":{"S":"a"},"sk":{"N":"1"},"v":{"S":"q\"uote"},"l":{"L":[{"N":"2"},{"B":"AQI="}]}}`
	s.ok(t, opPut, `{"TableName":"tab","Item":`+item+`}`)

	got := s.call(t, usEast1, opStatement, `{"Statement":"SELECT * FROM \"tab\" WHERE pk = 'a'"}`)
	require.JSONEq(t, `{"Items":[`+item+`]}`, got.body)

	got = s.call(t, usEast1, opStatement, `{"Statement":"SELECT * FROM \"tab\" WHERE pk = 'a' AND sk = 1"}`)
	require.JSONEq(t, `{"Items":[`+item+`]}`, got.body)

	got = s.call(t, usEast1, opBatchStmt,
		`{"Statements":[{"Statement":"SELECT * FROM \"tab\" WHERE pk = 'a' AND sk = 1"}]}`)
	require.JSONEq(t, `{"Responses":[{"TableName":"tab","Item":`+item+`}]}`, got.body)

	got = s.call(t, usEast1, opGet, `{"TableName":"tab","Key":{"pk":{"S":"a"},"sk":{"N":"1"}}}`)
	require.JSONEq(t, `{"Item":`+item+`}`, got.body)
}

// Test_packed_writesDuringReads: writers replace and delete items while
// readers write responses that hold the stored bytes and Snapshot copies the
// table. Run under -race, a write that touched bytes a response still reads
// would fail; every response is one whole item version.
func Test_packed_writesDuringReads(t *testing.T) {
	const (
		rounds = 300
		keys   = 4
	)

	s := newStack(t)
	s.ok(t, opCreate, fmt.Sprintf(packedTable, "tab"))

	var wg sync.WaitGroup

	for w := range 2 {
		wg.Go(func() {
			for i := range rounds {
				sk := strconv.Itoa(i % keys)
				v := strings.Repeat(strconv.Itoa(w), 64)

				if i%5 == 4 {
					s.call(t, usEast1, opDeleteItem, `{"TableName":"tab","Key":{"pk":{"S":"a"},"sk":{"N":"`+sk+`"}}}`)

					continue
				}

				s.call(t, usEast1, opPut, `{"TableName":"tab","Item":{"pk":{"S":"a"},"sk":{"N":"`+sk+
					`"},"v":{"S":"`+v+`"},"w":{"S":"`+v+`"}}}`)
			}
		})
	}

	reads := []struct{ op, body string }{
		{opGet, `{"TableName":"tab","Key":{"pk":{"S":"a"},"sk":{"N":"1"}}}`},
		{opQuery, `{"TableName":"tab","KeyConditionExpression":"pk = :h","ExpressionAttributeValues":{":h":{"S":"a"}}}`},
		{opScan, `{"TableName":"tab"}`},
		{opBatchGet, `{"RequestItems":{"tab":{"Keys":[{"pk":{"S":"a"},"sk":{"N":"2"}}]}}}`},
	}

	for _, read := range reads {
		wg.Go(func() {
			for range rounds {
				got := s.call(t, usEast1, read.op, read.body)
				if got.status != 200 {
					t.Errorf("%s answered %d: %s", read.op, got.status, got.body)

					return
				}

				// Each item's v and w come from the same put.
				for _, w := range []string{"0", "1"} {
					if strings.Count(got.body, strings.Repeat(w, 64))%2 != 0 {
						t.Errorf("%s answered a torn item: %s", read.op, got.body)

						return
					}
				}
			}
		})
	}

	wg.Go(func() {
		for range rounds / 10 {
			var buf bytes.Buffer
			err := s.provider.Snapshot(&buf)
			if err != nil {
				t.Error(err)

				return
			}
		}
	})

	wg.Wait()
}

// Test_packed_putsStoreTheirItem: every write that stores the request's item
// as it is, which packs it before the table's lock, stores that item: PutItem,
// a BatchWriteItem put, a TransactWriteItems Put, and the PartiQL INSERT of
// ExecuteStatement, BatchExecuteStatement and ExecuteTransaction. A put whose
// condition fails leaves the stored item as it was.
func Test_packed_putsStoreTheirItem(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, fmt.Sprintf(packedTable, "tab"))

	item := func(sk int) string {
		return `{"pk":{"S":"a"},"sk":{"N":"` + strconv.Itoa(sk) + `"},"v":{"S":"q\"uote ` + strconv.Itoa(sk) +
			`"},"l":{"L":[{"N":"2"},{"B":"AQI="}]},"m":{"M":{"b":{"BOOL":true}}}}`
	}
	insert := func(sk int) string {
		return `INSERT INTO \"tab\" VALUE {'pk':'a','sk':` + strconv.Itoa(sk) + `,'v':'q\"uote ` + strconv.Itoa(sk) + `'}`
	}

	s.ok(t, opPut, `{"TableName":"tab","Item":`+item(1)+`}`)
	s.ok(t, opBatchWrite, `{"RequestItems":{"tab":[{"PutRequest":{"Item":`+item(2)+`}},`+
		`{"PutRequest":{"Item":`+item(3)+`}}]}}`)
	s.ok(t, opTransactWrite, `{"TransactItems":[{"Put":{"TableName":"tab","Item":`+item(4)+`,`+
		`"ConditionExpression":"attribute_not_exists(pk)"}},{"Put":{"TableName":"tab","Item":`+item(5)+`}}]}`)
	s.ok(t, opStatement, `{"Statement":"`+insert(6)+`"}`)
	s.ok(t, opBatchStmt, `{"Statements":[{"Statement":"`+insert(7)+`"}]}`)
	s.ok(t, opTransaction, `{"TransactStatements":[{"Statement":"`+insert(8)+`"}]}`)

	s.ok(t, opPut, `{"TableName":"tab","Item":`+item(9)+`}`)

	refused := s.call(t, usEast1, opPut, `{"TableName":"tab","Item":`+
		strings.Replace(item(9), `"q\"uote 9"`, `"replaced"`, 1)+`,"ConditionExpression":"attribute_not_exists(pk)"}`)
	require.Contains(t, refused.body, "ConditionalCheckFailedException")

	statement := func(sk int) string {
		return `{"pk":{"S":"a"},"sk":{"N":"` + strconv.Itoa(sk) + `"},"v":{"S":"q\"uote ` + strconv.Itoa(sk) + `"}}`
	}

	for sk := 1; sk <= 9; sk++ {
		want := item(sk)
		if sk >= 6 && sk <= 8 {
			want = statement(sk)
		}

		got := s.call(t, usEast1, opGet, `{"TableName":"tab","Key":{"pk":{"S":"a"},"sk":{"N":"`+strconv.Itoa(sk)+`"}}}`)
		require.JSONEq(t, `{"Item":`+want+`}`, got.body, "item %d", sk)
	}
}

// Test_packed_requestNotRetained: an item put from a request padded to 64 KiB
// keeps none of the request: the heap grows by about the items, not by the
// bodies they came in.
func Test_packed_requestNotRetained(t *testing.T) {
	const (
		items   = 200
		padding = 64 << 10
	)

	s := newStack(t)
	s.ok(t, opCreate, fmt.Sprintf(packedTable, "tab"))

	pad := strings.Repeat(" ", padding)
	before := heapInUse()

	for i := range items {
		got := s.call(t, usEast1, opPut, `{"TableName":"tab",`+pad+`"Item":{"pk":{"S":"a"},"sk":{"N":"`+
			strconv.Itoa(i)+`"},"v":{"S":"`+pad[:16]+`x"}}}`)
		require.Equal(t, 200, got.status, got.body)
	}

	grown := heapInUse() - before
	// Each item is under 100 bytes; a retained body would add 64 KiB.
	require.Less(t, grown, int64(items*padding/8), "heap grew %d bytes for %d items", grown, items)

	runtime.KeepAlive(s)
}

func heapInUse() int64 {
	var m runtime.MemStats

	runtime.GC() //nolint:revive // call-to-gc: the test measures the live heap
	runtime.GC() //nolint:revive // call-to-gc: the test measures the live heap
	runtime.ReadMemStats(&m)

	return int64(m.HeapAlloc)
}
