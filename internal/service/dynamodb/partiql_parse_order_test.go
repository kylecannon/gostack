//nolint:unqueryvet // the statements under test are PartiQL, SELECT * included
package dynamodb_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The answers every row below was taken from: the LocalStack image
// (gostack-parity-localstack:8b9a79f) given the statement on a table "ptord"
// that holds the item {id: "a", x: 1, m: {x: 5}} (SQ-418, verification/SQ-418).
const (
	ordNotWellFormed = "Statement wasn't well formed, can't be processed: "
	ordTableName     = "Table name should be within the length [3, 255] and only contain " +
		"'a-z', 'A-Z', '0-9', '-', '_', '.'."
	ordUnrecognized  = "Unrecognized function: foo"
	ordAt1613        = "1:16:3"
	ordAliasing      = "Aliasing is not supported"
	ordUnsupportedTx = "Unsupported operation in transaction."
	ordExistsOnly    = "EXISTS can only be used in ExecuteTransaction write requests."
	ordTxPrefix      = "Validation failed in TransactStatements[0]: "
)

// ordError is the message of an answer, ordBatch the messages of a batch's.
type ordError struct {
	Message string `json:"message"`
}

type ordBatch struct {
	Responses []ordBatchResponse `json:"Responses"`
}

type ordBatchResponse struct {
	Error ordError `json:"Error"`
}

// ordText is src as a JSON string, its backquotes single quotes.
func ordText(t *testing.T, src string) string {
	t.Helper()

	b, err := json.Marshal(strings.ReplaceAll(src, "`", "'"))
	require.NoError(t, err)

	return string(b)
}

// ordBody is the body of ExecuteStatement answering src with 200.
func ordBody(t *testing.T, s *stack, src string) string {
	t.Helper()

	got := s.call(t, usEast1, opStatement, `{"Statement":`+ordText(t, src)+`}`)
	require.Equal(t, http.StatusOK, got.status, "%s: %s", src, got.body)

	return got.body
}

func ordStack(t *testing.T) *stack {
	t.Helper()

	s := newStack(t)
	s.ok(t, opCreate, hashTable("ptord"))
	s.ok(t, opPut, `{"TableName":"ptord","Item":{"id":{"S":"a"},"x":{"N":"1"},"m":{"M":{"x":{"N":"5"}}}}}`)

	return s
}

// ordMessages runs src through ExecuteStatement, BatchExecuteStatement and
// ExecuteTransaction and returns the message each answered with.
func ordMessages(t *testing.T, s *stack, src string) (single, batch, tx string) {
	t.Helper()

	text := ordText(t, src)

	one := s.call(t, usEast1, opStatement, `{"Statement":`+text+`}`)
	require.Equal(t, http.StatusBadRequest, one.status, "%s: %s", src, one.body)

	var single1 ordError

	require.NoError(t, json.Unmarshal([]byte(one.body), &single1), src)

	many := s.call(t, usEast1, "BatchExecuteStatement", `{"Statements":[{"Statement":`+text+`}]}`)
	require.Equal(t, http.StatusOK, many.status, "%s: %s", src, many.body)

	var batch1 ordBatch

	require.NoError(t, json.Unmarshal([]byte(many.body), &batch1), src)
	require.Len(t, batch1.Responses, 1, src)

	all := s.call(t, usEast1, opTransaction, `{"TransactStatements":[{"Statement":`+text+`}]}`)

	var tx1 ordError

	require.NoError(t, json.Unmarshal([]byte(all.body), &tx1), src)

	return single1.Message, batch1.Responses[0].Error.Message, tx1.Message
}

// Test_partiql_refusedFirstRows: what the image names before the table, the
// key, the index and the clause RETURNING, in all three calls. The statement's
// own expressions come first for a write, and the table's name, an alias and
// the paths of the list for a read.
func Test_partiql_refusedFirstRows(t *testing.T) {
	s := ordStack(t)
	ov := overlapMessage("path one: [a, b], path two: [a]")

	rows := []struct{ name, src, want string }{
		{"update, missing table", "UPDATE nosuch SET a = 1 SET a.b = 2 WHERE id = `a`", ov},
		{"update, no key", "UPDATE ptord SET a = 1 SET a.b = 2 WHERE q = 1", ov},
		{"update, no where", "UPDATE ptord SET a = 1 SET a.b = 2", ov},
		{"update, key of the wrong type", "UPDATE ptord SET a = 1 SET a.b = 2 WHERE id = 1", ov},
		{"update, key twice", "UPDATE ptord SET a = 1 SET a.b = 2 WHERE id = `a` AND id = `b`", ov},
		{"update, name of a table", "UPDATE x SET a = 1 SET a.b = 2 WHERE id = `a`", ov},
		{"update, remove and child", "UPDATE nosuch REMOVE a REMOVE a.b WHERE id = `a`", ov},
		{
			"update, a path twice", "UPDATE nosuch SET a = 1 SET a = 2 WHERE id = `a`",
			overlapMessage("path one: [a], path two: [a]"),
		},
		{"update, function", "UPDATE nosuch SET a = foo(1) WHERE id = `a`", ordUnrecognized},
		{
			"update, condition's function", "UPDATE nosuch SET a = 1 SET a.b = 2 WHERE id = `a` AND foo(x)",
			ordUnrecognized,
		},
		{
			"update, operator count",
			"UPDATE nosuch SET a = 1 WHERE id = `a` AND " + strings.Repeat("x = 1 AND ", 160) + "x = 1",
			"The expression contains too many operators; operator count: 301",
		},
		{
			"update, a sum in the condition", "UPDATE nosuch SET a = 1 WHERE id = `a` AND x = y + 1",
			"Unsupported operator in Condition Expression. Operator: +",
		},
		{"delete, function", "DELETE FROM nosuch WHERE id = `a` AND foo(x)", ordUnrecognized},
		{"select, missing table", "SELECT a, a.b FROM nosuch WHERE id = `a`", ov},
		{"select, no key", "SELECT a, a.b FROM ptord WHERE q = 1", ov},
		{"select, missing index", "SELECT a, a.b FROM ptord.nosuch WHERE id = `a`", ov},
		{"select, order by with no where", "SELECT a, a.b FROM ptord ORDER BY id", ov},
		{"select, name of a table first", "SELECT a, a.b FROM x WHERE id = `a`", ordTableName},
	}

	for _, row := range rows {
		single, batch, tx := ordMessages(t, s, row.src)
		require.Equal(t, row.want, single, row.name)
		require.Equal(t, row.want, batch, row.name)
		require.Equal(t, ordTxPrefix+row.want, tx, row.name)
	}
}

// Test_partiql_returningInATransactionAfterTheOverlap: a RETURNING clause is
// refused in a transaction after the statement's own expressions, which a
// missing table or a missing key does not come before.
func Test_partiql_returningInATransactionAfterTheOverlap(t *testing.T) {
	s := ordStack(t)
	ov := ordTxPrefix + overlapMessage("path one: [a, b], path two: [a]")

	for _, src := range []string{
		"UPDATE ptord SET a = 1 SET a.b = 2 WHERE id = `a` RETURNING ALL NEW *",
		"UPDATE nosuch SET a = 1 SET a.b = 2 WHERE id = `a` RETURNING ALL NEW *",
		"UPDATE ptord SET a = 1 SET a.b = 2 WHERE q = 1 RETURNING ALL NEW *",
	} {
		got := s.call(t, usEast1, opTransaction, `{"TransactStatements":[{"Statement":`+ordText(t, src)+`}]}`)
		require.Equal(t, http.StatusBadRequest, got.status, got.body)
		require.JSONEq(t, validation(ov), got.body, src)
	}
}

// Test_partiql_commaRemove: a REMOVE takes one path, and a comma after it is
// not well formed, where SET takes a list.
func Test_partiql_commaRemove(t *testing.T) {
	s := ordStack(t)

	for _, src := range []string{
		"UPDATE ptord REMOVE x, c WHERE id = `a`",
		"UPDATE ptord REMOVE x, c, m.x WHERE id = `a`",
		"UPDATE ptord SET x = 5 REMOVE c, m.y WHERE id = `a`",
		"UPDATE nosuch REMOVE a, b WHERE id = `a`",
		"UPDATE ptord REMOVE a, WHERE id = `a`",
		"update ptord remove x, c where id = 'a'",
	} {
		single, batch, tx := ordMessages(t, s, src)
		want := ordNotWellFormed + strings.ReplaceAll(src, "`", "'")
		require.Equal(t, want, single, src)
		require.Equal(t, want, batch, src)
		require.Equal(t, want, tx, src)
	}

	// One REMOVE for each path, and a SET list, are what the image takes.
	for _, src := range []string{
		"UPDATE ptord REMOVE c REMOVE x WHERE id = `a`",
		"UPDATE ptord REMOVE c SET x = 2, y = 3 WHERE id = `a`",
	} {
		ordBody(t, s, src)
	}
}

// Test_partiql_pathStepInQuotes: m['x'], m["x"] and m[x] are the path m.x, in a
// SELECT list, a WHERE and an UPDATE.
func Test_partiql_pathStepInQuotes(t *testing.T) {
	s := ordStack(t)

	for _, step := range []string{"m['x']", `m["x"]`, "m[x]", "m.x"} {
		got := ordBody(t, s, "SELECT "+step+" FROM ptord WHERE id = 'a'")
		require.JSONEq(t, `{"Items":[{"x":{"N":"5"}}]}`, got, step)

		got = ordBody(t, s, "SELECT id FROM ptord WHERE "+step+" = 5")
		require.JSONEq(t, `{"Items":[{"id":{"S":"a"}}]}`, got, step)
	}

	single, _, _ := ordMessages(t, s, "UPDATE ptord SET m['x'] = 7 SET m.x = 8 WHERE id = `a`")
	require.Equal(t, overlapMessage("path one: [m, x], path two: [m, x]"), single)

	single, _, _ = ordMessages(t, s, "UPDATE ptord SET m['x'] = 7 SET m = 8 WHERE id = `a`")
	require.Equal(t, overlapMessage("path one: [m], path two: [m, x]"), single)

	single, _, _ = ordMessages(t, s, "SELECT m['x'], m.x FROM ptord WHERE id = `a`")
	require.Equal(t, "Duplicate identifiers in select clause: x", single)

	single, _, _ = ordMessages(t, s, "SELECT m[``] FROM ptord WHERE id = `a`")
	require.Equal(t, "Path component cannot be an empty string", single)
}

// Test_partiql_aliasRefused: the image parses an alias and refuses it after the
// statement's own expressions and before its table, where a table's name comes
// before an alias of the list and after one of the table.
func Test_partiql_aliasRefused(t *testing.T) {
	s := ordStack(t)

	rows := []struct{ name, src, want string }{
		{"as", "SELECT x AS z FROM ptord WHERE id = `a`", ordAliasing},
		{"bare", "SELECT x z FROM ptord WHERE id = `a`", ordAliasing},
		{"quoted", `SELECT x "z" FROM ptord WHERE id = 'a'`, ordAliasing},
		{"second path", "SELECT x, m.x AS d FROM ptord WHERE id = `a`", ordAliasing},
		{"table", "SELECT x FROM ptord t WHERE id = `a`", ordAliasing},
		{"table as", "SELECT x FROM ptord AS t WHERE id = `a`", ordAliasing},
		{"missing table", "SELECT x AS z FROM nosuch WHERE id = `a`", ordAliasing},
		{"no key", "SELECT x AS z FROM ptord WHERE q = 1", ordAliasing},
		{"missing index", "SELECT x AS z FROM ptord.nosuch WHERE id = `a`", ordAliasing},
		{"name of the table, list alias", "SELECT x AS z FROM x WHERE id = `a`", ordTableName},
		{"name of the table, table alias", "SELECT x FROM x t WHERE id = `a`", ordAliasing},
		{"over an overlap", "SELECT a AS z, a.b FROM ptord WHERE id = `a`", ordAliasing},
		{"update with alias", "UPDATE ptord AS t SET a = 1 WHERE id = `a`", ordAliasing},
		{"update, missing table", "UPDATE nosuch t SET a = 1 WHERE id = `a`", ordAliasing},
		{
			"update, after the overlap", "UPDATE ptord AS t SET a = 1 SET a.b = 2 WHERE id = `a`",
			overlapMessage("path one: [a, b], path two: [a]"),
		},
		{"update, after a function", "UPDATE ptord AS t SET a = foo(1) WHERE id = `a`", ordUnrecognized},
		{"delete, no key", "DELETE FROM ptord t WHERE q = 1", ordAliasing},
		{
			"as with no name", "SELECT x AS FROM ptord WHERE id = `a`",
			ordNotWellFormed + "SELECT x AS FROM ptord WHERE id = 'a'",
		},
		{"star", "SELECT * AS z FROM ptord WHERE id = `a`", ordNotWellFormed + "SELECT * AS z FROM ptord WHERE id = 'a'"},
		{
			"string", "SELECT x AS `z` FROM ptord WHERE id = `a`",
			ordNotWellFormed + "SELECT x AS 'z' FROM ptord WHERE id = 'a'",
		},
	}

	for _, row := range rows {
		single, batch, tx := ordMessages(t, s, row.src)
		require.Equal(t, row.want, single, row.name)
		require.Equal(t, row.want, batch, row.name)

		wantTx := ordTxPrefix + row.want
		if strings.HasPrefix(row.want, ordNotWellFormed) {
			wantTx = row.want
		}

		require.Equal(t, wantTx, tx, row.name)
	}
}

// Test_partiql_aliasInATransaction: the mixing check of a transaction comes
// before an alias, which is named for the statement it is in.
func Test_partiql_aliasInATransaction(t *testing.T) {
	s := ordStack(t)
	read := "SELECT x FROM ptord WHERE id = 'a'"

	got := s.call(t, usEast1, opTransaction, `{"TransactStatements":[{"Statement":`+
		ordText(t, "UPDATE ptord SET a = 1 WHERE id = 'zz'")+`},{"Statement":`+
		ordText(t, "SELECT x AS z FROM ptord WHERE id = 'a'")+`}]}`)
	require.Equal(t, http.StatusBadRequest, got.status, got.body)
	require.JSONEq(t,
		validation("ExecuteTransaction API does not support both read and write operations in the same request."),
		got.body)

	got = s.call(t, usEast1, opTransaction, `{"TransactStatements":[{"Statement":`+ordText(t, read)+`},{"Statement":`+
		ordText(t, "SELECT x AS z FROM ptord WHERE id = 'a'")+`}]}`)
	require.Equal(t, http.StatusBadRequest, got.status, got.body)
	require.JSONEq(t, validation("Validation failed in TransactStatements[1]: "+ordAliasing), got.body)
}

// Test_partiql_blockComments: a /* */ comment is white space wherever white
// space may stand, ends at the first */ and is never nested, and a column
// counts the characters before it, comment included.
func Test_partiql_blockComments(t *testing.T) {
	s := ordStack(t)
	want := `{"Items":[{"id":{"S":"a"},"x":{"N":"1"},"m":{"M":{"x":{"N":"5"}}}}]}`

	for _, src := range []string{
		"SELECT /* c */ * FROM ptord /**/ WHERE id /* c */ = /* d */ 'a'",
		"/* x */ SELECT * /* a\n b */ FROM ptord WHERE id = 'a' /* y */",
		"SELECT * FROM ptord /*/ x */ WHERE id = 'a'",
		"SELECT * FROM ptord /***/ WHERE id = 'a'; /* z */",
		"SELECT * FROM ptord WHERE id = 'a'/* c */",
		"SELECT * FROM ptord WHERE id = 'a' /* -- */",
		"SELECT * FROM ptord WHERE id = 'a' -- /* \n",
	} {
		require.JSONEq(t, want, ordBody(t, s, src), src)
	}

	for _, src := range []string{
		"SELECT * FROM ptord WHERE id = 'a' /* open",
		"SELECT * FROM ptord /*/ WHERE id = 'a'",
		"SELECT * FROM ptord /* /* nested */ */ WHERE id = 'a'",
		"SEL/**/ECT * FROM ptord WHERE id = 'a'", //nolint:misspell // a keyword split by a comment
	} {
		single, _, _ := ordMessages(t, s, src)
		require.Equal(t, ordNotWellFormed+src, single, src)
	}

	for src, at := range map[string]string{
		"SELECT /* c */ foo(x) FROM ptord WHERE id = 'a'":          ordAt1613,
		"SELECT /* a\nb\nc */ foo(x) FROM ptord WHERE id = 'a'":    "3:6:3",
		"SELECT foo(x) /* c */ FROM ptord WHERE id = 'a'":          "1:8:3",
		"SELECT /* é */ foo(x) FROM ptord WHERE id = 'a'":          ordAt1613,
		"SELECT /* \U0001F600 */ foo(x) FROM ptord WHERE id = 'a'": ordAt1613,
		"SELECT -- é\n foo(x) FROM ptord WHERE id = 'a'":           "2:2:3",
	} {
		single, _, _ := ordMessages(t, s, src)
		require.Equal(t, "Unexpected path component at "+at, single, src)
	}
}

// Test_partiql_existsInATransaction: an EXISTS around something that is not a
// SELECT is an operation a transaction does not support, where outside one it
// is an EXISTS refused, and what is not an expression at all is not well formed.
func Test_partiql_existsInATransaction(t *testing.T) {
	s := ordStack(t)

	for _, src := range []string{
		"EXISTS(garbage)", "EXISTS()", "EXISTS(a = (1 + 2))", "EXISTS(NULL)", "EXISTS(NOT a)", "EXISTS(1)",
		"EXISTS(garbage);",
		"EXISTS(a, b)", "EXISTS('s')", "EXISTS(INSERT INTO ptord VALUE {'id': 'a'})", "exists(garbage)", "EXISTS(?)",
		"EXISTS(a.b[1])", "EXISTS(-- c\ngarbage)", "EXISTS(/* c */ garbage)", "EXISTS(SET a = 1)", "EXISTS(REMOVE a)",
		"EXISTS ( garbage )", "EXISTS(foo(1))", "EXISTS(<<1>>)",
	} {
		_, batch, tx := ordMessages(t, s, src)
		require.Equal(t, ordExistsOnly, batch, src)
		require.Equal(t, ordUnsupportedTx, tx, src)
	}

	for _, src := range []string{
		"EXISTS(WHERE id = 'a')", "EXISTS(RETURNING ALL OLD *)", "EXISTS(ORDER BY a)", "EXISTS(,)", "EXISTS(@)",
		"EXISTS(UPDATE ptord SET a = 1 WHERE id = 'a')", "EXISTS(garbage) x", "EXISTS(garbage",
	} {
		_, batch, tx := ordMessages(t, s, src)
		require.Equal(t, ordNotWellFormed+src, batch, src)
		require.Equal(t, ordNotWellFormed+src, tx, src)
	}
}
