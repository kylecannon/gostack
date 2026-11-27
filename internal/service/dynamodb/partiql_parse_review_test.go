//nolint:unqueryvet // the statements under test are PartiQL, SELECT * included
package dynamodb_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The answers the rows below were taken from: the LocalStack image
// (gostack-parity-localstack:8b9a79f), SQ-418 review round 1, verification/SQ-418
// p2a, p2b and p2c. The statements run on the table "ptord" of ordStack.
const (
	revEmptyStep   = "Path component cannot be an empty string"
	revDistinct    = "Unsupported token in expression: DISTINCT"
	revBatchMixed  = "Read and write requests together in the same batch is not supported."
	revTxMixed     = "ExecuteTransaction API does not support both read and write operations in the same request."
	revTxPrefix1   = "Validation failed in TransactStatements[1]: "
	revSingleItem  = "Only single item select is supported"
	revParamsCount = "Number of parameters in request and statement don't match."
	revNoTable     = "Cannot do operations on a non-existent table"
	revTxSelectKey = "Select statements within ExecuteTransaction must specify the primary key in the where clause."
	revSelEmpty    = "SELECT m[''] FROM ptord WHERE id = 'a'"
	revUpdateX     = "UPDATE ptord SET x = 9 WHERE id = 'a'"
)

// revMessages is ordMessages for a statement that may succeed in a batch or a
// transaction: the message of each call, "" when it answered 200.
func revMessages(t *testing.T, s *stack, src string) (single, batch, tx string) {
	t.Helper()

	text := ordText(t, src)

	var e ordError

	one := s.call(t, usEast1, opStatement, `{"Statement":`+text+`}`)
	if one.status != http.StatusOK {
		require.NoError(t, json.Unmarshal([]byte(one.body), &e), src)
		single = e.Message
	}

	many := s.call(t, usEast1, "BatchExecuteStatement", `{"Statements":[{"Statement":`+text+`}]}`)
	require.Equal(t, http.StatusOK, many.status, "%s: %s", src, many.body)

	var b ordBatch

	require.NoError(t, json.Unmarshal([]byte(many.body), &b), src)
	require.Len(t, b.Responses, 1, src)

	batch = b.Responses[0].Error.Message

	all := s.call(t, usEast1, opTransaction, `{"TransactStatements":[{"Statement":`+text+`}]}`)
	if all.status != http.StatusOK {
		e = ordError{}

		require.NoError(t, json.Unmarshal([]byte(all.body), &e), src)
		tx = e.Message
	}

	return single, batch, tx
}

// revCall runs several statements as a batch (batch true) or a transaction and
// returns the status and the message of a refused call.
//
//nolint:revive // flag-parameter: a batch or a transaction
func revCall(t *testing.T, s *stack, batch bool, srcs ...string) (int, string) {
	t.Helper()

	items := make([]string, len(srcs))
	for i, src := range srcs {
		items[i] = `{"Statement":` + ordText(t, src) + `}`
	}

	op, field := opTransaction, "TransactStatements"
	if batch {
		op, field = "BatchExecuteStatement", "Statements"
	}

	got := s.call(t, usEast1, op, `{"`+field+`":[`+strings.Join(items, ",")+`]}`)

	var e ordError

	_ = json.Unmarshal([]byte(got.body), &e)

	return got.status, e.Message
}

// revAll requires want of the three calls, a transaction's with its prefix.
func revAll(t *testing.T, s *stack, src, want string) {
	t.Helper()

	single, batch, tx := revMessages(t, s, src)
	require.Equal(t, want, single, src)
	require.Equal(t, want, batch, src)
	require.Equal(t, ordTxPrefix+want, tx, src)
}

// Test_partiql_emptyStepAfterTheParse: a path step that names an empty string
// is refused where the path is used, in the order of the statement's
// expressions, not by the parser: an unfinished step is not well formed, and in
// a batch or a transaction the statement has a kind (it can mix reads and
// writes) and a transaction names its index.
func Test_partiql_emptyStepAfterTheParse(t *testing.T) {
	s := ordStack(t)

	for _, src := range []string{
		"SELECT m[' FROM ptord WHERE id = 'a'", "SELECT m[''  FROM ptord WHERE id = 'a'", "SELECT m['']",
		"UPDATE ptord SET m['' = 1 WHERE id = 'a'",
	} {
		single, batch, _ := revMessages(t, s, src)
		require.Equal(t, ordNotWellFormed+src, single, src)
		require.Equal(t, ordNotWellFormed+src, batch, src)
	}

	for _, src := range []string{
		revSelEmpty, "SELECT m[\"\"] FROM ptord WHERE id = 'a'",
		"SELECT \"\" FROM ptord WHERE id = 'a'", "SELECT m.\"\" FROM ptord WHERE id = 'a'",
		"SELECT a.m[''] FROM ptord WHERE id = 'a'", "SELECT m[0][''] FROM ptord WHERE id = 'a'",
		"SELECT m[''] FROM nosuch WHERE id = 'a'", "SELECT m[''] FROM ptord WHERE q = 1",
		"SELECT m[''] FROM ptord.nosuch WHERE id = 'a'",
		"SELECT m[''] FROM ptord WHERE id = 'a' AND foo(x)", "SELECT * FROM ptord WHERE id = 'a' AND m[''] = 1",
		"SELECT * FROM ptord WHERE m[''] = 1 AND foo(x)", "SELECT a, a.b, m[''] FROM ptord WHERE id = 'a'",
		"SELECT m[''], a, a.b FROM ptord WHERE id = 'a'", "SELECT ALL m[''] FROM ptord WHERE id = 'a'",
		"UPDATE ptord SET m[''] = 1 WHERE id = 'a'", "UPDATE nosuch SET m[''] = 1 WHERE id = 'a'",
		"UPDATE ptord REMOVE m[''] WHERE id = 'a'", "UPDATE ptord SET \"\" = 1 WHERE id = 'a'",
		"UPDATE ptord SET m[''] = 1 WHERE id = 'a' RETURNING ALL NEW *", "UPDATE ptord t SET m[''] = 1 WHERE id = 'a'",
		"UPDATE ptord SET m[''] = foo(1) WHERE id = 'a'", "UPDATE ptord SET m[''] = 1 SET m = 2 WHERE id = 'a'",
		"DELETE FROM ptord WHERE id = 'a' AND m[''] = 1", "DELETE FROM ptord t WHERE m[''] = 1",
		"DELETE FROM ptord WHERE id = 'a' AND \"\" = 1 RETURNING ALL OLD *",
	} {
		revAll(t, s, src, revEmptyStep)
	}

	// An expression read before the step is named first, and an alias of the
	// list before any.
	revAll(t, s, "SELECT * FROM ptord WHERE foo(x) AND m[''] = 1", ordUnrecognized)
	revAll(t, s, "UPDATE ptord SET a = foo(1), m[''] = 1 WHERE id = 'a'", ordUnrecognized)
	revAll(t, s, "SELECT m[''] FROM \"a\" WHERE id = 'a'", ordTableName)
	revAll(t, s, "SELECT m[''] AS z FROM ptord WHERE id = 'a'", ordAliasing)
	revAll(t, s, "SELECT m[''] FROM ptord t WHERE id = 'a'", ordAliasing)

	// A statement that leaves the step unread is not refused for it.
	_, batch, tx := revMessages(t, s, "SELECT * FROM ptord WHERE id = 'a' ORDER BY m['']")
	require.Empty(t, batch)
	require.Empty(t, tx)

	got := s.call(t, usEast1, opTransaction, `{"TransactStatements":[{"Statement":`+
		ordText(t, "EXISTS(SELECT m[''] FROM ptord WHERE id = 'a' AND x = 1)")+`}]}`)
	require.Equal(t, http.StatusOK, got.status, got.body)

	// The statement has a kind: a read among writes is a mixed call, and the
	// write before it is not applied.
	for _, batch := range []bool{true, false} {
		want := revTxMixed
		if batch {
			want = revBatchMixed
		}

		for _, srcs := range [][]string{
			{revUpdateX, revSelEmpty},
			{revSelEmpty, revUpdateX},
			{revUpdateX, "SELECT m[''] FROM ptord WHERE id = ?"},
		} {
			status, msg := revCall(t, s, batch, srcs...)
			require.Equal(t, http.StatusBadRequest, status, srcs)
			require.Equal(t, want, msg, srcs)
			require.JSONEq(t, `{"Items":[{"x":{"N":"1"}}]}`, ordBody(t, s, "SELECT x FROM ptord WHERE id = 'a'"))
		}
	}

	// In a transaction the statement that is refused is named by its index.
	status, msg := revCall(t, s, false, revUpdateX,
		"UPDATE ptord SET m[''] = 1 WHERE id = 'b'")
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, revTxPrefix1+revEmptyStep, msg)
	require.JSONEq(t, `{"Items":[{"x":{"N":"1"}}]}`, ordBody(t, s, "SELECT x FROM ptord WHERE id = 'a'"))
}

// Test_partiql_selectWhereBeforeTheKey: a SELECT's own WHERE is read before the
// table, the index and the key, in a batch and a transaction as in
// ExecuteStatement, which the image does for all three: its function is named
// before a WHERE that holds no key, and before a table that is not there.
func Test_partiql_selectWhereBeforeTheKey(t *testing.T) {
	s := ordStack(t)

	for _, src := range []string{
		"SELECT * FROM ptord WHERE q = 1 AND foo(x)", "SELECT * FROM ptord WHERE foo(x)",
		"SELECT x FROM ptord WHERE q = 1 AND foo(x)", "SELECT * FROM ptord WHERE id = 'a' AND foo(x)",
		"SELECT * FROM nosuch WHERE id = 'a' AND foo(x)", "SELECT * FROM nosuch WHERE q = 1 AND foo(x)",
		"SELECT * FROM ptord.nosuch WHERE q = 1 AND foo(x)",
	} {
		revAll(t, s, src, ordUnrecognized)
	}

	// What is not wrong with the WHERE is answered as it was.
	_, batch, tx := revMessages(t, s, "SELECT * FROM ptord WHERE q = 1")
	require.Equal(t, revSingleItem, batch)
	require.Equal(t, ordTxPrefix+revTxSelectKey, tx)

	// The list's overlap comes before the WHERE's function.
	revAll(t, s, "SELECT a, a.b FROM ptord WHERE q = 1 AND foo(x)", overlapMessage("path one: [a, b], path two: [a]"))
}

// Test_partiql_conditionTextAgrees: a SELECT's list names placeholders first, in
// ExecuteStatement as in a batch and a transaction, so the text of an error of
// the rendered expressions is the same in each (the WHERE below is gostack's
// own syntax error for a bare operand, which no refusal of the image is named
// for, a difference listed in docs/compat.md); and the count of operators holds
// with and without the table: 299 are run, 301 and 303 are refused with their
// count, in each call.
func Test_partiql_conditionTextAgrees(t *testing.T) {
	s := ordStack(t)

	// Each text is the statement's, whether the table is there or not (with none
	// the table is named instead, below); a SELECT's list takes the first
	// placeholders, so its x and y are #n0 and #n1 and the bare operand #n3.
	for src, want := range map[string]string{
		"DELETE FROM ptord WHERE id = 'a' AND x":                `Syntax error; token: "<EOF>", near: "#n0"`,
		"DELETE FROM ptord WHERE id = 'a' AND x AND y":          `Syntax error; token: "AND", near: ") AND ("`,
		"UPDATE ptord SET a = 1 WHERE id = 'a' AND (x OR y)":    `Syntax error; token: "OR", near: ") OR ("`,
		"DELETE FROM ptord WHERE id = 'a' AND x = 1 AND y":      `Syntax error; token: "<EOF>", near: ")"`,
		"SELECT * FROM ptord WHERE id = 'a' AND x AND id = 'b'": `Syntax error; token: ")", near: ")) AND"`,
		"SELECT x, y FROM ptord WHERE id = 'a' AND z":           `Syntax error; token: "<EOF>", near: "#n3"`,
	} {
		single, _, _ := revMessages(t, s, src)
		require.Equal(t, want, single, src)
	}

	for _, src := range []string{
		"DELETE FROM nosuch WHERE id = 'a' AND x", "UPDATE nosuch SET a = 1 WHERE id = 'a' AND (x OR y)",
		"SELECT x, y FROM nosuch WHERE id = 'a' AND z",
	} {
		single, batch, tx := revMessages(t, s, src)
		require.Equal(t, revNoTable, single, src)
		require.Equal(t, revNoTable, batch, src)
		require.Equal(t, "Requested resource not found", tx, src)
	}
}

// Test_partiql_aliasBeforeReturningInATransaction: a statement's alias is
// refused after its own expressions and before the RETURNING clause, which a
// transaction refuses after the table and the key.
func Test_partiql_aliasBeforeReturningInATransaction(t *testing.T) {
	s := ordStack(t)

	for _, src := range []string{
		"UPDATE ptord AS t SET a = 1 WHERE id = 'a' RETURNING ALL NEW *",
		"UPDATE ptord t SET a = 1 WHERE id = 'a' RETURNING ALL NEW *",
		"DELETE FROM ptord t WHERE id = 'a' RETURNING ALL OLD *",
		"UPDATE nosuch AS t SET a = 1 WHERE id = 'a' RETURNING ALL NEW *",
		"UPDATE ptord AS t SET a = 1 WHERE q = 1 RETURNING ALL NEW *",
	} {
		revAll(t, s, src, ordAliasing)
	}

	revAll(t, s, "UPDATE ptord AS t SET a = foo(1) WHERE id = 'a' RETURNING ALL NEW *", ordUnrecognized)
}

// Test_partiql_existsAlias: an EXISTS refuses its table's alias before
// anything else of it, and reads the alias of its list for nothing.
func Test_partiql_existsAlias(t *testing.T) {
	s := ordStack(t)

	refused := []string{
		"EXISTS(SELECT x AS z FROM ptord u WHERE id = 'a')", "EXISTS(SELECT x FROM ptord u WHERE id = 'a' AND x = 1)",
		"EXISTS(SELECT * FROM ptord u WHERE id = 'a' AND x = 1)", "EXISTS(SELECT x FROM ptord AS u WHERE id = 'a' AND x = 1)",
		"EXISTS(SELECT x AS z FROM nosuch u WHERE id = 'a')",
		"EXISTS(SELECT x AS z, x.y FROM ptord u WHERE id = 'a' AND x = 1)",
		"EXISTS(SELECT x FROM ptord u WHERE id = 'a' AND foo(x))", "EXISTS(SELECT * FROM ptord u WHERE q = 1 AND x = 1)",
		"EXISTS(SELECT x FROM \"a\" u WHERE id = 'a' AND x = 1)",
		"EXISTS(SELECT x FROM ptord.nosuch u WHERE id = 'a' AND x = 1)",
		"EXISTS(SELECT x FROM ptord u WHERE id = 'a' AND m[''] = 1)",
	}
	for _, src := range refused {
		got := s.call(t, usEast1, opTransaction, `{"TransactStatements":[{"Statement":`+ordText(t, src)+`}]}`)
		require.Equal(t, http.StatusBadRequest, got.status, src)
		require.JSONEq(t, validation(ordTxPrefix+ordAliasing), got.body, src)
	}

	for _, src := range []string{
		"EXISTS(SELECT x AS z FROM ptord WHERE id = 'a' AND x = 1)", "EXISTS(SELECT x FROM ptord WHERE id = 'a' AND x = 1)",
		"EXISTS(SELECT a, a.b FROM ptord WHERE id = 'a' AND x = 1)",
		"EXISTS(SELECT m[''] FROM ptord WHERE id = 'a' AND x = 1)",
		"EXISTS(SELECT DISTINCT x FROM ptord WHERE id = 'a' AND x = 1)",
		"EXISTS(SELECT ALL x FROM ptord WHERE id = 'a' AND x = 1)",
	} {
		got := s.call(t, usEast1, opTransaction, `{"TransactStatements":[{"Statement":`+ordText(t, src)+`}]}`)
		require.Equal(t, http.StatusOK, got.status, "%s: %s", src, got.body)
	}
}

// Test_partiql_distinctAndAll: ALL is the default quantifier of a SELECT and
// changes nothing, where DISTINCT is refused before anything else of the
// statement but its mixing with others; both, and the keywords the image
// reserves beside them, are not names.
func Test_partiql_distinctAndAll(t *testing.T) {
	s := ordStack(t)

	for _, src := range []string{
		"SELECT DISTINCT x FROM ptord WHERE id = 'a'", "SELECT DISTINCT * FROM ptord WHERE id = 'a'",
		"SELECT distinct x FROM ptord WHERE id = 'a'", "SELECT DISTINCT x AS z FROM ptord", "SELECT DISTINCT x FROM ptord t",
		"SELECT DISTINCT a, a.b FROM ptord", "SELECT DISTINCT x FROM \"a\"", "SELECT DISTINCT x FROM ptord WHERE foo(x)",
		"SELECT DISTINCT x FROM ptord.nosuch", "SELECT DISTINCT x FROM ptord ORDER BY x", "SELECT DISTINCT m[''] FROM ptord",
		"SELECT DISTINCT x FROM ptord WHERE id = ?", "SELECT DISTINCT x FROM nosuch",
	} {
		single, batch, tx := revMessages(t, s, src)
		require.Equal(t, revDistinct, single, src)
		require.Equal(t, revDistinct, batch, src)
		require.Equal(t, ordTxPrefix+revDistinct, tx, src)
	}

	for src, want := range map[string]string{
		"SELECT ALL x FROM ptord t WHERE id = 'a'": ordAliasing,
		"SELECT ALL x AS z FROM ptord":             ordAliasing,
		"SELECT ALL a, a.b FROM ptord":             overlapMessage("path one: [a, b], path two: [a]"),
		"SELECT ALL x FROM nosuch":                 revNoTable,
		"SELECT ALL x FROM ptord WHERE foo(x)":     ordUnrecognized,
		"SELECT ALL x FROM ptord.nosuch":           "The table does not have the specified index: nosuch",
	} {
		single, _, _ := revMessages(t, s, src)
		require.Equal(t, want, single, src)
	}

	for _, src := range []string{
		"SELECT ALL x FROM ptord WHERE id = 'a'", "SELECT all x FROM ptord WHERE id = 'a'",
		"SELECT ALL * FROM ptord WHERE id = 'a'", "SELECT \"ALL\" FROM ptord WHERE id = 'a'",
	} {
		_, batch, tx := revMessages(t, s, src)
		require.Empty(t, batch, src)
		require.Empty(t, tx, src)
	}

	require.JSONEq(t, `{"Items":[{"x":{"N":"1"}}]}`, ordBody(t, s, "SELECT ALL x FROM ptord WHERE id = 'a'"))
	require.JSONEq(t, `{"Items":[{}]}`, ordBody(t, s, "SELECT \"ALL\" FROM ptord WHERE id = 'a'"))

	for _, src := range []string{
		"SELECT ALL DISTINCT x FROM ptord", "SELECT DISTINCT ALL x FROM ptord", "SELECT ALL " + "ALL x FROM ptord",
		"SELECT ALL FROM ptord", "SELECT DISTINCT FROM ptord", "SELECT ALL, x FROM ptord", "SELECT x, ALL y FROM ptord",
		"SELECT x, DISTINCT y FROM ptord", "SELECT x.ALL FROM ptord WHERE id = 'a'", "SELECT * FROM ptord WHERE ALL = 1",
		"SELECT * FROM ptord WHERE id = 'a' ORDER BY ALL", "SELECT x FROM ptord ALL WHERE id = 'a'",
		"UPDATE ptord SET ALL = 1 WHERE id = 'a'", "DELETE FROM ptord ALL WHERE id = 'a'", "SELECT AS FROM ptord",
		"SELECT x AS DISTINCT FROM ptord", "SELECT AS x FROM ptord", "SELECT MODIFIED FROM ptord", "SELECT ONLY FROM ptord",
		"SELECT UNPIVOT FROM ptord", "SELECT PIVOT FROM ptord", "SELECT OLD FROM ptord", "SELECT NEW FROM ptord",
		"SELECT ASC FROM ptord", "SELECT DESC FROM ptord", "SELECT LIMIT FROM ptord",
		"SELECT * FROM ptord WHERE id = 'a' AND LIMIT = 1",
	} {
		single, batch, _ := revMessages(t, s, src)
		require.Equal(t, ordNotWellFormed+src, single, src)
		require.Equal(t, ordNotWellFormed+src, batch, src)
	}

	// A DISTINCT is a read among writes, and what the call is refused for.
	for _, batch := range []bool{true, false} {
		want := revTxMixed
		if batch {
			want = revBatchMixed
		}

		status, msg := revCall(t, s, batch, revUpdateX, "SELECT DISTINCT x FROM ptord WHERE id = 'a'")
		require.Equal(t, http.StatusBadRequest, status)
		require.Equal(t, want, msg)
	}

	status, msg := revCall(t, s, false, "SELECT DISTINCT x FROM ptord WHERE id = 'a'",
		"SELECT x FROM ptord WHERE id = 'b'")
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, ordTxPrefix+revDistinct, msg)
}

// Test_partiql_tokenLengthInCharacters: the length of a token in a message
// counts characters, as its column does (image: 1:34:3 for 'é').
func Test_partiql_tokenLengthInCharacters(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("tbl"))

	got := ordBody2(t, s, "INSERT INTO tbl VALUE {'é': 'e', 'é': 'f'}")
	require.Equal(t, "Duplicate keys in tuple. Key: é under root at 1:34:3", got)

	got = ordBody2(t, s, "INSERT INTO tbl VALUE {'id': 'é', 'é': 'e', 'é': 'f'}")
	require.Equal(t, "Duplicate keys in tuple. Key: é under root at 1:45:3", got)

	//nolint:gosmopolitan // a multi-byte key, probed (p2a t5-dup3)
	got = ordBody2(t, s, "INSERT INTO tbl VALUE {'日本': 'e', 'x': 'f', 'x': 1}")
	require.Equal(t, "Duplicate keys in tuple. Key: x under root at 1:45:3", got)
}

// ordBody2 is the message of ExecuteStatement answering src with 400.
func ordBody2(t *testing.T, s *stack, src string) string {
	t.Helper()

	got := s.call(t, usEast1, opStatement, `{"Statement":`+ordText(t, src)+`}`)
	require.Equal(t, http.StatusBadRequest, got.status, "%s: %s", src, got.body)

	var e ordError

	require.NoError(t, json.Unmarshal([]byte(got.body), &e), src)

	return e.Message
}
