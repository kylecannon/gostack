//nolint:unqueryvet // the statements under test are PartiQL, SELECT * included
package dynamodb_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The answers the rows of this file were taken from: the LocalStack image
// (gostack-parity-localstack:8b9a79f), SQ-418 review round 2, verification/SQ-418
// p4a, p4b, p4c, p4e, p4f and p4g. The statements run on the table "ptord" of
// ordStack, keyed by the string id.
const (
	wrapNotBoolean = "WHERE clause must contain an expression that resolves to a boolean"
	wrapKeyPath    = "Key attributes must be scalars; list random access '[]' and map lookup '.' are not allowed: Key"
	wrapOrderPath  = "Argument to ORDER BY must be of type VariableReference. Type: PATH"
	wrapWhereKeys  = "Where clause does not contain a mandatory equality on all key attributes"
	wrapInvalidTok = "Invalid NextToken"
	wrapTxNotFound = "Requested resource not found"
	wrapTable      = "ptord"
	wrapMXOverlap  = "Two document paths overlap with each other; must remove or rewrite one of these paths; " +
		"path one: [m, x], path two: [m]"
	wrapNoWhere = "Must have WHERE clause in the statement when using ORDER BY clause."
)

// wrapItem is the item ordStack puts, which a statement that deletes or updates
// it takes away or changes.
const wrapItem = `{"TableName":"ptord","Item":{"id":{"S":"a"},"x":{"N":"1"},"m":{"M":{"x":{"N":"5"}}}}}`

// Test_partiql_ownErrorsBeforeEverythingElse: a statement's own error is named
// before a missing table, a missing key, a key of the wrong type and a missing
// index, in ExecuteStatement, BatchExecuteStatement and ExecuteTransaction,
// whichever of them reaches the refusal: the order is made once for the three
// (refusedInOrder). The rows hold a SELECT, an UPDATE and a DELETE with each
// kind of own error against each refusal that follows it.
func Test_partiql_ownErrorsBeforeEverythingElse(t *testing.T) {
	s := ordStack(t)

	// An own error of the WHERE, which every kind holds.
	tails := []struct{ name, want, tail string }{
		{"function", ordUnrecognized, "foo(x)"},
		{"sum", "Unsupported operator in Condition Expression. Operator: +", "x = y + 1"},
		{"empty step", revEmptyStep, "m[''] = 1"},
		{
			"operator count", "The expression contains too many operators; operator count: 301",
			strings.Repeat("x = 1 AND ", 149) + "x = 1",
		},
	}

	// The refusals after them: a key of the wrong type, no key, no table.
	heads := []struct{ table, key string }{
		{wrapTable, "id = 1 AND "},
		{wrapTable, "q = 1 AND "},
		{"nosuch", "id = 'a' AND "},
		{wrapTable + ".nosuch", "id = 'a' AND "},
	}

	for _, o := range tails {
		for _, h := range heads {
			where := h.key + o.tail

			for _, src := range []string{
				"SELECT * FROM " + h.table + " WHERE " + where,
				"UPDATE " + h.table + " SET a = 1 WHERE " + where,
				"DELETE FROM " + h.table + " WHERE " + where,
			} {
				if strings.HasPrefix(src, "SELECT") || !strings.Contains(h.table, ".") {
					revAll(t, s, src, o.want)
				}
			}
		}
	}

	// A SELECT's list and an UPDATE's actions are own errors too.
	for _, h := range heads {
		for _, src := range []string{
			"SELECT a, a.b FROM " + h.table + " WHERE " + h.key + "x = 1",
			"UPDATE " + h.table + " SET a = 1 SET a.b = 2 WHERE " + h.key + "x = 1",
		} {
			if strings.HasPrefix(src, "SELECT") || !strings.Contains(h.table, ".") {
				revAll(t, s, src, overlapMessage("path one: [a, b], path two: [a]"))
			}
		}
	}

	// The SELECT of the review: a key of the wrong type, then a function.
	revAll(t, s, "SELECT * FROM ptord WHERE id = 1 AND foo(x)", ordUnrecognized)
	revAll(t, s, "SELECT * FROM ptord WHERE q = 1 AND foo(x)", ordUnrecognized)
	revAll(t, s, "SELECT * FROM ptord.nosuch WHERE id = 1 AND foo(x)", ordUnrecognized)

	// The name of the table is a SELECT's own, before the rest of it.
	revAll(t, s, "SELECT * FROM x WHERE id = 1 AND foo(x)", ordTableName)
}

// wrapSurfaces runs src through the three calls, re-putting the item before
// each, and returns the message each answered with, "" for 200.
func wrapSurfaces(t *testing.T, s *stack, src string) (single, batch, tx string) {
	t.Helper()

	text := ordText(t, src)
	msg := func(status int, body string) string {
		if status == http.StatusOK {
			return ""
		}

		var e ordError

		require.NoError(t, json.Unmarshal([]byte(body), &e), src)

		return e.Message
	}

	s.ok(t, opPut, wrapItem)

	one := s.call(t, usEast1, opStatement, `{"Statement":`+text+`}`)
	single = msg(one.status, one.body)

	s.ok(t, opPut, wrapItem)

	many := s.call(t, usEast1, "BatchExecuteStatement", `{"Statements":[{"Statement":`+text+`}]}`)
	require.Equal(t, http.StatusOK, many.status, "%s: %s", src, many.body)

	var b ordBatch

	require.NoError(t, json.Unmarshal([]byte(many.body), &b), src)
	require.Len(t, b.Responses, 1, src)

	batch = b.Responses[0].Error.Message

	s.ok(t, opPut, wrapItem)

	all := s.call(t, usEast1, opTransaction, `{"TransactStatements":[{"Statement":`+text+`}]}`)
	tx = msg(all.status, all.body)

	return single, batch, tx
}

// Test_partiql_whereIsNotACondition: a WHERE that is a path, a value or size()
// answers "WHERE clause must contain an expression that resolves to a boolean"
// before the table, the key, the clause RETURNING, an alias and an UPDATE's
// overlap, as the image does; a SELECT's own list comes before it. The same
// inputs answered gostack's own syntax error for the rendered text of an
// operand, or the key's refusal on main.
func Test_partiql_whereIsNotACondition(t *testing.T) {
	s := ordStack(t)

	for _, src := range []string{
		"DELETE FROM ptord WHERE x", "UPDATE ptord SET a = 1 WHERE x", "DELETE FROM nosuch WHERE x",
		"DELETE FROM ptord WHERE TRUE", "DELETE FROM ptord WHERE FALSE", "DELETE FROM ptord WHERE 1",
		"DELETE FROM ptord WHERE 'a'", "DELETE FROM ptord WHERE NULL", "DELETE FROM ptord WHERE (x)",
		"DELETE FROM ptord WHERE size(x)", "DELETE FROM ptord WHERE {'a': 1}", "DELETE FROM ptord WHERE <<1>>",
		"DELETE FROM ptord WHERE [1]", "DELETE FROM ptord WHERE m.x", "DELETE FROM ptord WHERE m['x']",
		"UPDATE nosuch SET a = 1 WHERE x", "UPDATE ptord SET id = 1 WHERE x", "UPDATE ptord SET a = 1, a = 2 WHERE x",
		"UPDATE ptord AS t SET a = 1 WHERE x", "UPDATE ptord SET a = 1 WHERE x RETURNING ALL NEW *",
		"DELETE FROM ptord WHERE x RETURNING ALL OLD *",
		"SELECT * FROM ptord WHERE x", "SELECT * FROM nosuch WHERE x", "SELECT * FROM ptord.nosuch WHERE x",
		"SELECT * FROM ptord WHERE TRUE", "SELECT * FROM ptord WHERE size(x)", "SELECT a FROM ptord WHERE x",
	} {
		revAll(t, s, src, wrapNotBoolean)
	}

	// An EXISTS, which only a transaction takes.
	for _, src := range []string{
		"EXISTS(SELECT * FROM ptord WHERE x)", "EXISTS(SELECT * FROM ptord WHERE TRUE)",
		"EXISTS(SELECT * FROM nosuch WHERE x)",
	} {
		status, msg := revCall(t, s, false, src)
		require.Equal(t, http.StatusBadRequest, status, src)
		require.Equal(t, ordTxPrefix+wrapNotBoolean, msg, src)
	}

	// What comes before it in the image, and what is not wrong.
	revAll(t, s, "SELECT a, a.b FROM ptord WHERE x", overlapMessage("path one: [a, b], path two: [a]"))
	revAll(t, s, "SELECT a AS z FROM ptord WHERE x", ordAliasing)
	revAll(t, s, revSelEmpty+" AND x", revEmptyStep)
	revAll(t, s, "DELETE FROM ptord WHERE foo(x)", ordUnrecognized)
	revAll(t, s, "DELETE FROM ptord WHERE x + 1", "Unsupported operator in Condition Expression. Operator: +")
	revAll(t, s, "DELETE FROM ptord WHERE MISSING", "MISSING must be compared using IS operator")

	// A condition is a condition.
	single, batch, tx := wrapSurfaces(t, s, "DELETE FROM ptord WHERE begins_with(x, 'a')")
	require.Equal(t, wrapWhereKeys, single)
	require.Equal(t, wrapWhereKeys, batch)
	require.Contains(t, tx, wrapWhereKeys)
}

// Test_partiql_syntaxOfRenderedTextLeavesTheOriginal: a bare operand under an AND,
// OR or NOT is a syntax error of the expression the statement is rendered to,
// which no answer of the image's names ("The arguments to AND/OR must be
// Expressions", a difference listed in docs/compat.md). It must not take the
// place of the refusal that was there: no key, no table.
func Test_partiql_syntaxOfRenderedTextLeavesTheOriginal(t *testing.T) {
	s := ordStack(t)

	for _, tail := range []string{"q = 1 AND x", "x OR y", "NOT x", "q = 1 AND TRUE", "x AND id = 'a' OR y"} {
		for _, head := range []string{"DELETE FROM ptord WHERE ", "UPDATE ptord SET a = 1 WHERE "} {
			single, batch, tx := wrapSurfaces(t, s, head+tail)
			require.Equal(t, wrapWhereKeys, single, head+tail)
			require.Equal(t, wrapWhereKeys, batch, head+tail)
			require.Contains(t, tx, wrapWhereKeys, head+tail)
			require.NotContains(t, tx, "Syntax error", head+tail)

			single, batch, tx = wrapSurfaces(t, s, strings.Replace(head, "ptord", "nosuch", 1)+tail)
			require.Equal(t, revNoTable, single, head+tail)
			require.Equal(t, revNoTable, batch, head+tail)
			require.Equal(t, wrapTxNotFound, tx, head+tail)
		}
	}

	// A SELECT of no key is a Scan in ExecuteStatement, and not a single item
	// in a batch or a transaction.
	single, batch, tx := wrapSurfaces(t, s, "SELECT * FROM nosuch WHERE q = 1 AND x")
	require.Equal(t, revNoTable, single)
	require.Equal(t, revNoTable, batch)
	require.Equal(t, wrapTxNotFound, tx)

	_, batch, tx = wrapSurfaces(t, s, "SELECT * FROM ptord WHERE q = 1 AND x")
	require.Equal(t, revSingleItem, batch)
	require.Equal(t, ordTxPrefix+revTxSelectKey, tx)
}

// Test_partiql_pathBelowAKey: a WHERE that goes below an attribute of the key
// (id['x'], id.x, id[0]) answers "Key attributes must be scalars" before the
// key's equality is asked for; ExecuteStatement's SELECT answers it too, with
// a colon, after the table and the token, and a batch and a transaction answer
// a SELECT as they did.
func Test_partiql_pathBelowAKey(t *testing.T) {
	s := ordStack(t)

	for _, src := range []string{
		"DELETE FROM ptord WHERE id['x'] = 'a'", "DELETE FROM ptord WHERE id.x = 'a'",
		"DELETE FROM ptord WHERE id[0] = 'a'", "DELETE FROM ptord WHERE id['x'] > 'a'",
		"DELETE FROM ptord WHERE 'a' = id['x']", "DELETE FROM ptord WHERE begins_with(id['x'], 'a')",
		"DELETE FROM ptord WHERE id['x'] IN ('a')", "DELETE FROM ptord WHERE id['x'] = 'a' AND q = 1",
		"DELETE FROM ptord WHERE id = 'a' AND id['x'] = 'b'", "DELETE FROM ptord WHERE id['x'] = 'b' AND id = 'a'",
		"UPDATE ptord SET a = 1 WHERE id['x'] = 'a'", "UPDATE ptord SET a = 1 WHERE id['x'] = 'a' RETURNING ALL NEW *",
		"DELETE FROM ptord WHERE id['x'] = 'a' RETURNING ALL OLD *",
	} {
		revAll(t, s, src, wrapKeyPath+" id")
	}

	status, msg := revCall(t, s, false, "EXISTS(SELECT * FROM ptord WHERE id['x'] = 'a' AND x = 1)")
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, ordTxPrefix+wrapKeyPath+" id", msg)

	// The own errors and the table come first; a path below another attribute
	// is no business of the key's.
	revAll(t, s, "DELETE FROM ptord WHERE id['x'] = 'a' AND foo(y)", ordUnrecognized)

	single, batch, tx := wrapSurfaces(t, s, "DELETE FROM nosuch WHERE id['x'] = 'a'")
	require.Equal(t, revNoTable, single)
	require.Equal(t, revNoTable, batch)
	require.Equal(t, wrapTxNotFound, tx)

	single, batch, tx = wrapSurfaces(t, s, "DELETE FROM ptord WHERE x['y'] = 1")
	require.Equal(t, wrapWhereKeys, single)
	require.Equal(t, wrapWhereKeys, batch)
	require.Contains(t, tx, wrapWhereKeys)

	// A SELECT.
	for _, src := range []string{
		"SELECT * FROM ptord WHERE id['x'] = 'a'", "SELECT * FROM ptord WHERE id.x = 'a'",
		"SELECT * FROM ptord WHERE id[0] = 'a'", "SELECT * FROM ptord WHERE id['x'] > 'a'",
		"SELECT * FROM ptord WHERE NOT id['x'] = 'a'", "SELECT * FROM ptord WHERE id['x'] IS NULL",
		"SELECT * FROM ptord WHERE size(id['x']) > 1", "SELECT * FROM ptord WHERE id['x'] = 'a' OR q = 1",
		"SELECT * FROM ptord WHERE id['x'] = 'a' ORDER BY x", "SELECT * FROM ptord WHERE id['x'] = 'a' ORDER BY m['x']",
	} {
		single, batch, tx = wrapSurfaces(t, s, src)
		require.Equal(t, wrapKeyPath+": id", single, src)
		require.Equal(t, revSingleItem, batch, src)
		require.Equal(t, ordTxPrefix+revTxSelectKey, tx, src)
	}

	single, _, _ = wrapSurfaces(t, s, "SELECT * FROM ptord WHERE x['y'] = 1")
	require.Empty(t, single)

	// The token before it, and the table, and the own errors.
	got := s.call(t, usEast1, opStatement, `{"Statement":"SELECT * FROM ptord WHERE id['x'] = 'a'","NextToken":"bad"}`)
	require.Contains(t, got.body, wrapInvalidTok)

	got = s.call(t, usEast1, opStatement, `{"Statement":"SELECT * FROM ptord WHERE foo(x)","NextToken":"bad"}`)
	require.Contains(t, got.body, ordUnrecognized)

	got = s.call(t, usEast1, opStatement, `{"Statement":"SELECT * FROM ptord WHERE x","NextToken":"bad"}`)
	require.Contains(t, got.body, wrapNotBoolean)

	got = s.call(t, usEast1, opStatement, `{"Statement":"SELECT * FROM nosuch WHERE id['x'] = 'a'","NextToken":"bad"}`)
	require.Contains(t, got.body, revNoTable)
}

// Test_partiql_keyPathOfTwoKeys: the first path of a write's WHERE below a key
// attribute names it, in the order the WHERE writes them; a SELECT names the
// hash key first.
func Test_partiql_keyPathOfTwoKeys(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, `{"TableName":"prange","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},`+
		`{"AttributeName":"r","KeyType":"RANGE"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"},`+
		`{"AttributeName":"r","AttributeType":"N"}],"ProvisionedThroughput":{"ReadCapacityUnits":5,"WriteCapacityUnits":5}}`)
	s.ok(t, opPut, `{"TableName":"prange","Item":{"id":{"S":"a"},"r":{"N":"1"},"x":{"N":"1"}}}`)

	for src, want := range map[string]string{
		"DELETE FROM prange WHERE r['x'] = 1 AND id['x'] = 'a'":   " r",
		"DELETE FROM prange WHERE id['x'] = 'a' AND r['x'] = 1":   " id",
		"DELETE FROM prange WHERE id = 'a' AND r['x'] = 1":        " r",
		"DELETE FROM prange WHERE id['x'] = 'a' AND r = 1":        " id",
		"SELECT * FROM prange WHERE r['x'] = 1 AND id['x'] = 'a'": ": id",
		"SELECT * FROM prange WHERE id['x'] = 'a' AND r['x'] = 1": ": id",
		"SELECT * FROM prange WHERE id = 'a' AND r['x'] > 1":      ": r",
	} {
		single, _, _ := revMessages(t, s, src)
		require.Equal(t, wrapKeyPath+want, single, src)
	}

	single, _, _ := revMessages(t, s, "SELECT * FROM prange WHERE id = 'a' AND x['y'] = 1")
	require.Empty(t, single)
}

// Test_partiql_orderByAPath: an ORDER BY of a path (m['x'], m.x, l[0], id.x)
// answers "Argument to ORDER BY must be of type VariableReference. Type: PATH"
// in ExecuteStatement, which a batch and a transaction leave unread, after the
// own errors, the table, the clause's missing WHERE and the token, and after a
// key path in the WHERE.
func Test_partiql_orderByAPath(t *testing.T) {
	s := ordStack(t)

	for _, order := range []string{"m['x']", "m['']", "m.x", "m.x DESC", "l[0]", "id.x", "id[0]", "m['x'] ASC"} {
		src := "SELECT * FROM ptord WHERE id = 'a' ORDER BY " + order

		single, batch, tx := wrapSurfaces(t, s, src)
		require.Equal(t, wrapOrderPath, single, src)
		require.Empty(t, batch, src)
		require.Empty(t, tx, src)
	}

	// An attribute is no path.
	single, _, _ := wrapSurfaces(t, s, "SELECT * FROM ptord WHERE id = 'a' ORDER BY id")
	require.Empty(t, single)

	for src, want := range map[string]string{
		"SELECT * FROM ptord WHERE id = 'a' AND foo(x) ORDER BY m['x']":     ordUnrecognized,
		"SELECT a AS z FROM ptord WHERE id = 'a' ORDER BY m['x']":           ordAliasing,
		"SELECT m, m.x FROM ptord WHERE id = 'a' ORDER BY m['x']":           wrapMXOverlap,
		"SELECT * FROM nosuch WHERE id = 'a' ORDER BY m['x']":               revNoTable,
		"SELECT * FROM ptord ORDER BY m['x']":                               wrapNoWhere,
		"SELECT * FROM ptord WHERE id['x'] = 'a' ORDER BY m['x']":           wrapKeyPath + ": id",
		"SELECT * FROM ptord WHERE x = 1 AND id = 'a' ORDER BY m['x']":      wrapOrderPath,
		"SELECT * FROM ptord WHERE id = 'a' AND m['x'] = 1 ORDER BY m['x']": wrapOrderPath,
	} {
		single, _, _ = revMessages(t, s, src)
		require.Equal(t, want, single, src)
	}

	got := s.call(t, usEast1, opStatement,
		`{"Statement":"SELECT * FROM ptord WHERE id = 'a' ORDER BY m['x']","NextToken":"bad"}`)
	require.Contains(t, got.body, wrapInvalidTok)
}

// Test_partiql_operatorCountAtTheLimit: a statement's expressions hold 300
// operators. The WHERE below holds 2n+1 of them, so 149 (299) is run, and 150
// (301) and 151 (303) are refused, naming 301 where the count stops, in each
// call and whether the table is there or not (the image: SQ-454).
func Test_partiql_operatorCountAtTheLimit(t *testing.T) {
	s := ordStack(t)

	for _, n := range []int{149, 150, 151} {
		where := "id = 'a' AND " + strings.Repeat("x = 1 AND ", n-1) + "x = 1"
		count := 2*n + 1 // the operators of the WHERE

		for _, table := range []string{"ptord", "nosuch"} {
			single, batch, tx := wrapSurfaces(t, s, "DELETE FROM "+table+" WHERE "+where)

			switch {
			case count > 300:
				want := tooManyOperators
				require.Equal(t, want, single, n)
				require.Equal(t, want, batch, n)
				require.Equal(t, ordTxPrefix+want, tx, n)
			case table == "ptord":
				// Run: the item is there for each call (wrapSurfaces puts it).
				require.Empty(t, single, n)
				require.Empty(t, batch, n)
				require.Empty(t, tx, n)
			default:
				require.Equal(t, revNoTable, single, n)
				require.Equal(t, revNoTable, batch, n)
				require.Equal(t, wrapTxNotFound, tx, n)
			}
		}
	}
}

// Test_partiql_listNamesBeforeTheWhere: a SELECT's list takes the first
// placeholders of its expressions, in ExecuteStatement as in the read a batch and
// a transaction run. The WHERE below is gostack's own syntax error, whose text
// quotes #n3: the list's x and y are #n0 and #n1, id is #n2.
func Test_partiql_listNamesBeforeTheWhere(t *testing.T) {
	s := ordStack(t)

	single, batch, tx := wrapSurfaces(t, s, "SELECT x, y FROM ptord WHERE id = 'a' AND z")
	require.Equal(t, `Syntax error; token: "<EOF>", near: "#n3"`, single)
	require.Equal(t, revSingleItem, batch)
	require.Equal(t, ordTxPrefix+revTxSelectKey, tx)

	// A missing table is named, not the text of the rendered WHERE.
	single, batch, tx = wrapSurfaces(t, s, "SELECT x, y FROM nosuch WHERE id = 'a' AND z")
	require.Equal(t, revNoTable, single)
	require.Equal(t, revNoTable, batch)
	require.Equal(t, wrapTxNotFound, tx)
}
