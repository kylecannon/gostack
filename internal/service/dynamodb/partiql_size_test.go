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

// The image holds a PartiQL statement to its own 8,192 bytes and to nothing
// else: the SELECT list and the UPDATE actions of a statement run through a
// ProjectionExpression and an UpdateExpression that no size limit holds, where
// a request sent to the item operations is refused past 4,096 characters
// (SQ-412). The rows are what the LocalStack image answered, except those of
// Test_partiql_selectWideListGlobalIndex, which pin gostack's documented answer
// where the image refuses. They were checked in a differential of 1,222
// statements against the image, with a global index that projects some
// attributes asked only for the ones it projects.

// wideSizes are the lengths of a SELECT list the tests take. 602 distinct
// top-level attributes was the first the read refused (SQ-417).
var wideSizes = []int{601, 602, 603, 700, 850, 1000}

// wideItem is a PutItem of the item key, which holds n attributes k0 to
// k(n-1) with the value v<i>, and the attributes the key of an index needs.
func wideItem(table, key string, n int) string {
	var b strings.Builder

	fmt.Fprintf(&b, `{"TableName":%q,"Item":{%s`, table, key)

	for i := range n {
		fmt.Fprintf(&b, `,"k%d":{"S":"v%d"}`, i, i)
	}

	b.WriteString("}}")

	return b.String()
}

// wideCall is one way a SELECT of a single item, or of several, is sent.
type wideCall struct {
	op   string
	wrap func(stmt string) string
	item func(t *testing.T, body string) map[string]any
}

// wideCalls are the three surfaces a statement runs through. The batch and
// the transaction take a read of one item by its key alone.
func wideCalls() (statement, batched, transaction wideCall) {
	first := func(list string, member string) func(t *testing.T, body string) map[string]any {
		return func(t *testing.T, body string) map[string]any {
			t.Helper()

			var out map[string][]map[string]any

			require.NoError(t, json.Unmarshal([]byte(body), &out), body)
			require.Len(t, out[list], 1, body)

			item, ok := out[list][0][member].(map[string]any)
			require.True(t, ok, body)

			return item
		}
	}

	items := func(t *testing.T, body string) map[string]any {
		t.Helper()

		var out struct {
			Items []map[string]any `json:"Items"`
		}

		require.NoError(t, json.Unmarshal([]byte(body), &out), body)
		require.Len(t, out.Items, 1, body)

		return out.Items[0]
	}

	return wideCall{opStatement, func(s string) string { return s }, items},
		wideCall{opBatchStmt, func(s string) string { return `{"Statements":[` + s + `]}` }, first("Responses", "Item")},
		wideCall{
			opTransaction, func(s string) string { return `{"TransactStatements":[` + s + `]}` },
			first("Responses", "Item"),
		}
}

// requireWide requires that the answer to a SELECT of the first n attributes
// is the one item with each of them.
func requireWide(t *testing.T, s *stack, call wideCall, src string, n int) {
	t.Helper()

	got := s.call(t, usEast1, call.op, call.wrap(`{"Statement":`+jsonString(src)+`}`))
	require.Equal(t, http.StatusOK, got.status, "%s %d: %.300s", call.op, n, got.body)

	item := call.item(t, got.body)
	require.Len(t, item, n, "%s %d", call.op, n)
	require.Equal(t, map[string]any{"S": "v" + strconv.Itoa(n-1)}, item["k"+strconv.Itoa(n-1)], "%s %d", call.op, n)
}

// Test_partiql_selectWideList: a SELECT of 601 to 1,000 distinct top-level
// attributes answers the item with all of them, as the image does, on a Query,
// a Scan, and the batch and the transaction a read of one item goes through.
func Test_partiql_selectWideList(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("pwide"))
	s.ok(t, opPut, wideItem("pwide", `"id":{"S":"a"}`, 1200))

	statement, batched, transaction := wideCalls()

	for _, n := range wideSizes {
		list := selectList("k", n)

		requireWide(t, s, statement, "SELECT "+list+" FROM pwide WHERE id = 'a'", n)
		requireWide(t, s, statement, "SELECT "+list+" FROM pwide WHERE k0 = 'v0'", n)
		requireWide(t, s, batched, "SELECT "+list+" FROM pwide WHERE id = 'a'", n)
		requireWide(t, s, transaction, "SELECT "+list+" FROM pwide WHERE id = 'a'", n)
	}
}

// Test_partiql_selectWideListBelowTop: paths below the top level add nothing
// to the list the read runs with, and a list of them answers as it did.
func Test_partiql_selectWideListBelowTop(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("pwide"))
	s.ok(t, opPut, `{"TableName":"pwide","Item":{"id":{"S":"a"},"m":{"M":{"k0":{"S":"x"},"k999":{"S":"y"}}}}}`)

	got := s.call(t, usEast1, opStatement,
		`{"Statement":`+jsonString("SELECT "+selectList("m.k", 1000)+" FROM pwide WHERE id = 'a'")+`}`)
	require.Equal(t, http.StatusOK, got.status, got.body)
	require.JSONEq(t, `{"Items":[{"k0":{"S":"x"},"k999":{"S":"y"}}]}`, got.body)
}

// indexedTable is a CreateTable body of a table keyed by id and sk, with the
// local secondary indexes l_keys, l_incl and l_all and the global secondary
// indexes g_keys, g_incl and g_all, which project the keys only, the keys
// and k0, k5 and k700, and everything.
func indexedTable(name string) string {
	include := `"Projection":{"ProjectionType":"INCLUDE","NonKeyAttributes":["k0","k5","k700"]}`
	keysOnly := `"Projection":{"ProjectionType":"KEYS_ONLY"}`
	all := `"Projection":{"ProjectionType":"ALL"}`
	local := func(index, projection string) string {
		return `{"IndexName":"` + index + `","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},` +
			`{"AttributeName":"lk","KeyType":"RANGE"}],` + projection + `}`
	}
	global := func(index, projection string) string {
		return `{"IndexName":"` + index + `","KeySchema":[{"AttributeName":"gk","KeyType":"HASH"}],` + projection + `}`
	}

	return `{"TableName":"` + name + `","BillingMode":"PAY_PER_REQUEST","KeySchema":[` +
		`{"AttributeName":"id","KeyType":"HASH"},{"AttributeName":"sk","KeyType":"RANGE"}],"AttributeDefinitions":[` +
		`{"AttributeName":"id","AttributeType":"S"},{"AttributeName":"sk","AttributeType":"S"},` +
		`{"AttributeName":"gk","AttributeType":"S"},{"AttributeName":"lk","AttributeType":"S"}],` +
		`"LocalSecondaryIndexes":[` + local("l_keys", keysOnly) + `,` + local("l_incl", include) + `,` +
		local("l_all", all) + `],"GlobalSecondaryIndexes":[` + global("g_keys", keysOnly) + `,` +
		global("g_incl", include) + `,` + global("g_all", all) + `]}`
}

// Test_partiql_selectWideListIndexes: a SELECT of 700 to 1,000 attributes of a
// local secondary index answers every one of them, those the index does not
// project from the table's item, whether its projection is the keys only or
// the keys and three attributes, and so does one of a global index that
// projects all.
func Test_partiql_selectWideListIndexes(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, indexedTable("pidx"))
	s.ok(t, opPut, wideItem("pidx", `"id":{"S":"a"},"sk":{"S":"x"},"gk":{"S":"g"},"lk":{"S":"l"}`, 1200))

	statement, _, _ := wideCalls()

	for _, n := range []int{700, 850, 1000} {
		list := selectList("k", n)

		for _, index := range []string{"l_keys", "l_incl", "l_all"} {
			requireWide(t, s, statement, "SELECT "+list+` FROM "pidx"."`+index+`" WHERE id = 'a'`, n)
			requireWide(t, s, statement, "SELECT "+list+` FROM "pidx"."`+index+`"`, n)
			requireWide(t, s, statement, "SELECT "+list+` FROM "pidx"."`+index+`" WHERE id = 'a' AND lk = 'l'`, n)
		}

		requireWide(t, s, statement, "SELECT "+list+` FROM "pidx"."g_all" WHERE gk = 'g'`, n)
		requireWide(t, s, statement, "SELECT "+list+` FROM "pidx"."g_all"`, n)
	}
}

// Test_partiql_selectWideListGlobalIndex: a SELECT of 700 to 1,000 attributes
// of a global index that does not project them all is read as it is at any
// length. The image refuses an attribute the index does not project, "Global
// secondary index g_keys does not project [...]", where gostack answers the
// ones it does, as docs/compat.md says of a ProjectionExpression (SQ-412).
func Test_partiql_selectWideListGlobalIndex(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, indexedTable("pidx"))
	s.ok(t, opPut, wideItem("pidx", `"id":{"S":"a"},"sk":{"S":"x"},"gk":{"S":"g"},"lk":{"S":"l"}`, 1200))

	for _, n := range []int{700, 850, 1000} {
		list := "id,sk,gk," + selectList("k", n-3)

		// k700 is the one attribute besides k0 and k5 that g_incl projects.
		included := map[string]any{"id": nil, "sk": nil, "gk": nil, "k0": nil, "k5": nil}
		if n-3 > 700 {
			included["k700"] = nil
		}

		for index, want := range map[string]int{"g_keys": 3, "g_incl": len(included)} {
			got := s.call(t, usEast1, opStatement,
				`{"Statement":`+jsonString("SELECT "+list+` FROM "pidx"."`+index+`" WHERE gk = 'g'`)+`}`)
			require.Equal(t, http.StatusOK, got.status, "%s %d: %.300s", index, n, got.body)

			var out struct {
				Items []map[string]any `json:"Items"`
			}

			require.NoError(t, json.Unmarshal([]byte(got.body), &out), got.body)
			require.Len(t, out.Items, 1, "%s %d", index, n)
			require.Len(t, out.Items[0], want, "%s %d: %v", index, n, out.Items[0])
		}
	}
}

// setActions are n SET actions of u0 to u(n-1), each its own clause.
func setActions(n int) string {
	actions := make([]string, n)
	for i := range actions {
		actions[i] = "SET u" + strconv.Itoa(i) + " = 1"
	}

	return strings.Join(actions, " ")
}

// Test_partiql_updateWideActions: an UPDATE of 281 to 600 SET actions, the
// first the read of the expression it compiles to refused for its size, is
// applied as the image applies it, from ExecuteStatement, BatchExecuteStatement
// and ExecuteTransaction, in the clause form and the comma form.
func Test_partiql_updateWideActions(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("pupd"))

	statement, batched, transaction := wideCalls()
	commas := func(n int) string {
		actions := make([]string, n)
		for i := range actions {
			actions[i] = "u" + strconv.Itoa(i) + " = " + strconv.Itoa(i)
		}

		return "SET " + strings.Join(actions, ", ")
	}

	for _, n := range []int{280, 281, 300, 400, 600} {
		for _, call := range []wideCall{statement, batched, transaction} {
			for form, actions := range map[string]string{"clauses": setActions(n), "commas": commas(n)} {
				id := fmt.Sprintf("%s-%s-%d", call.op, form, n)
				s.ok(t, opPut, `{"TableName":"pupd","Item":{"id":{"S":"`+id+`"}}}`)

				src := "UPDATE pupd " + actions + " WHERE id = '" + id + "'"
				got := s.call(t, usEast1, call.op, call.wrap(`{"Statement":`+jsonString(src)+`}`))
				require.Equal(t, http.StatusOK, got.status, "%s %s %d: %.300s", call.op, form, n, got.body)
				require.NotContains(t, got.body, "Error", "%s %s %d", call.op, form, n)

				item := s.ok(t, opGet, `{"TableName":"pupd","Key":{"id":{"S":"`+id+`"}}}`)
				stored := path(t, item, "Item").(map[string]any) //nolint:forcetypeassert // decoded JSON object
				require.Len(t, stored, n+1, "%s %s %d", call.op, form, n)
			}
		}
	}
}

// Test_partiql_updateWideOverlap: an UPDATE of 400 actions that holds a path
// under one of them is refused for the overlap, as the image refuses it, and
// not for the size of the expression. The pair the message names is the one
// the image named: the statement's paths are added to its overlap trie first
// to last (SQ-395).
func Test_partiql_updateWideOverlap(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("pupd"))

	const overlap = "Two document paths overlap with each other; must remove or rewrite one of these paths; "

	run(t, s, []step{
		{
			"child last", opStatement, stmt("UPDATE pupd " + setActions(400) + " SET u5.q = 1 WHERE id = `a`"),
			http.StatusBadRequest, validation(overlap + "path one: [u5, q], path two: [u300]"),
		},
		{
			"child first", opStatement, stmt("UPDATE pupd SET u5.q = 1 " + setActions(400) + " WHERE id = `a`"),
			http.StatusBadRequest, validation(overlap + "path one: [u5], path two: [u3]"),
		},
	})
}

// Test_partiql_sizeStaysOnItemOperations: the lift is the statement's alone. A
// request sent to Scan, Query, GetItem or UpdateItem is refused for the size of
// its expression past 4,096 characters, as the image refuses it.
func Test_partiql_sizeStaysOnItemOperations(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("pwide"))
	s.ok(t, opPut, wideItem("pwide", `"id":{"S":"a"}`, 10))

	names := strings.ReplaceAll(selectList("k", 1000), ",", ", ")
	projection := `"ProjectionExpression":` + jsonString(names)
	projMessage := "Invalid ProjectionExpression: Expression size has exceeded the maximum allowed size;"

	actions := make([]string, 400)
	for i := range actions {
		actions[i] = "u" + strconv.Itoa(i) + " = :v"
	}

	updateBody := `{"TableName":"pwide","Key":{"id":{"S":"a"}},"UpdateExpression":` +
		jsonString("SET "+strings.Join(actions, ", ")) + `,"ExpressionAttributeValues":{":v":{"N":"1"}}}`

	s.fails(t, "Scan", `{"TableName":"pwide",`+projection+`}`, codeValidation, projMessage)
	s.fails(t, "Query", `{"TableName":"pwide","KeyConditionExpression":"id = :i",`+
		`"ExpressionAttributeValues":{":i":{"S":"a"}},`+projection+`}`, codeValidation, projMessage)
	s.fails(t, opGet, `{"TableName":"pwide","Key":{"id":{"S":"a"}},`+projection+`}`, codeValidation, projMessage)
	s.fails(t, "UpdateItem", updateBody, codeValidation,
		"Invalid UpdateExpression: Expression size has exceeded the maximum allowed size;")

	// A statement before them leaves them as strict.
	s.ok(t, opStatement, `{"Statement":`+jsonString("SELECT "+selectList("k", 700)+" FROM pwide WHERE id = 'a'")+`}`)
	s.fails(t, "Scan", `{"TableName":"pwide",`+projection+`}`, codeValidation, projMessage)
}
