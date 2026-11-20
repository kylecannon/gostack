//nolint:unqueryvet // the statements under test are PartiQL, SELECT * included
package dynamodb_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// noItems is an ExecuteStatement that read or returned nothing.
const noItems = `{"Items":[]}`

// statements is a body whose member lists the statements srcs, single quotes
// written as backquotes.
func statements(member string, srcs ...string) string {
	bodies := make([]string, len(srcs))
	for i, src := range srcs {
		bodies[i] = stmt(src)
	}

	return `{"` + member + `":[` + strings.Join(bodies, ",") + `]}`
}

// batch is a BatchExecuteStatement body of the statements srcs.
func batch(srcs ...string) string { return statements("Statements", srcs...) }

// transaction is an ExecuteTransaction body of the statements srcs.
func transaction(srcs ...string) string { return statements("TransactStatements", srcs...) }

// Test_partiql_batchDuplicateRuns: a batch that names one item twice runs
// its statements in order up to and including the repeat, then answers the
// duplicate error; the statements after it do not run (verification/SQ-229
// probe5, "dup first", "dup last", "dup insert pair").
func Test_partiql_batchDuplicateRuns(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("pdup"))
	s.ok(t, opPut, `{"TableName":"pdup","Item":{"id":{"S":"a"},"n":{"N":"1"}}}`)
	s.ok(t, opPut, `{"TableName":"pdup","Item":{"id":{"S":"b"},"n":{"N":"2"}}}`)

	dup := validation("Provided list of item keys contains duplicates")

	run(t, s, []step{{
		"repeat first", opBatchStmt, batch("UPDATE pdup SET x = 2 WHERE id = `a`", "UPDATE pdup SET x = 3 WHERE id = `a`",
			"INSERT INTO pdup VALUE {`id`: `d`}"), http.StatusBadRequest, dup,
	}})
	got := s.ok(t, opGet, `{"TableName":"pdup","Key":{"id":{"S":"a"}}}`)
	require.Equal(t, "3", path(t, got, "Item", "x", "N"), "the repeating statement ran")
	require.Equal(t, int64(1), s.changed.Load(), "a refused batch that wrote reports its change for persistence")
	require.False(t, s.present(t, "pdup", "d"), "the statement after the repeat did not run")

	run(t, s, []step{{
		"repeat last", opBatchStmt, batch("INSERT INTO pdup VALUE {`id`: `e`}", "UPDATE pdup SET y = 1 WHERE id = `b`",
			"DELETE FROM pdup WHERE id = `b`"), http.StatusBadRequest, dup,
	}})
	require.True(t, s.present(t, "pdup", "e"), "the statement before the repeat ran")
	require.False(t, s.present(t, "pdup", "b"), "the repeating DELETE ran")

	run(t, s, []step{
		{
			"failed insert still counts", opBatchStmt, batch("INSERT INTO pdup VALUE {`id`: `g`}",
				"INSERT INTO pdup VALUE {`id`: `g`, `v`: 2}"), http.StatusBadRequest, dup,
		},
		{
			"the first insert is the item", opGet, `{"TableName":"pdup","Key":{"id":{"S":"g"}}}`, http.StatusOK,
			`{"Item":{"id":{"S":"g"}}}`,
		},
	})

	before := s.changed.Load()

	run(t, s, []step{
		{
			"repeated read", opBatchStmt, batch("SELECT * FROM pdup WHERE id = `a`", "SELECT * FROM pdup WHERE id = `a`"),
			http.StatusBadRequest, dup,
		},
		{
			"repeated failed write", opBatchStmt, batch("UPDATE pdup SET x = 1 WHERE id = `nope`",
				"UPDATE pdup SET x = 2 WHERE id = `nope`"), http.StatusBadRequest, dup,
		},
	})
	require.Equal(t, before, s.changed.Load(), "a refused batch that wrote nothing reports no change")
}

// Test_partiql_imageMessages: the messages the LocalStack image answers for a
// statement's own mistakes in each operation (verification/SQ-229 probes 5
// and 6).
func Test_partiql_imageMessages(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("pmsg"))
	s.ok(t, opPut, `{"TableName":"pmsg","Item":{"id":{"S":"a"},"n":{"N":"1"},"nn":{"NS":["1"]},`+
		`"m":{"M":{"x":{"L":[{"N":"1"}]},"n":{"N":"3"}}}}}`)

	const (
		arity = "Incorrect number of operands for operator or function; " +
			"operator or function: begins_with, number of operands: 1"
		overlap  = "Two document paths overlap with each other; must remove or rewrite one of these paths; "
		setValue = "The second argument to SET_ADD must be a value with type SET at "
	)

	batchErr := func(msg string) string {
		return `{"Responses":[{"Error":{"Code":"ValidationError","Message":` + jsonString(msg) + `}}]}`
	}

	run(t, s, []step{
		{
			"batch lone name", opBatchStmt, batch("SELEC"), http.StatusOK,
			batchErr("Invalid operation. Only select, insert, update and delete operations are valid"),
		},
		{
			"statement lone name", opStatement, stmt("SELEC"), http.StatusBadRequest,
			validation("Unsupported operation: VariableReference"),
		},
		{
			"transaction names the statement", opTransaction,
			transaction("INSERT INTO pmsg VALUE {`id`: `t`}", "UPDATE pmsg SET y = 1 WHERE id = `a` AND begins_with(n)"),
			http.StatusBadRequest, validation("Validation failed in TransactStatements[1]: " + arity),
		},
		{
			"transaction read names the statement", opTransaction,
			transaction("SELECT * FROM pmsg WHERE id = `a` AND begins_with(n)"),
			http.StatusBadRequest, validation("Validation failed in TransactStatements[0]: " + arity),
		},
		{
			"batch read checks its filter first", opBatchStmt,
			batch("SELECT * FROM pmsg WHERE id = `a` AND begins_with(n)"), http.StatusOK, batchErr(arity),
		},
		{
			"size in an update", opStatement, stmt("UPDATE pmsg SET y = n + size(n) WHERE id = `a`"),
			http.StatusBadRequest, validation("The function is not allowed in an update expression; function: size"),
		},
		{
			"set_add of a number", opStatement, stmt("UPDATE pmsg SET nn = set_add(nn, 12) WHERE id = `a`"),
			http.StatusBadRequest, validation(setValue + "1:30:2"),
		},
		{
			"set_add of a number parameter", opStatement,
			stmt("UPDATE pmsg SET nn = set_add(nn, ?) WHERE id = `a`", `,"Parameters":[{"N":"5"}]`),
			http.StatusBadRequest, validation(setValue + "1:30:2"),
		},
		{
			"set_add of a set parameter", opStatement,
			stmt("UPDATE pmsg SET nn = set_add(nn, ?) WHERE id = `a`", `,"Parameters":[{"NS":["5"]}]`),
			http.StatusOK, noItems,
		},
		{
			"set_delete on the second line", opStatement,
			`{"Statement":"UPDATE pmsg\nSET nn = set_delete(nn, 1) WHERE id = 'a'"}`,
			http.StatusBadRequest, validation("The second argument to SET_DELETE must be a value with type SET at 2:21:2"),
		},
		{
			"duplicate identifier", opStatement, stmt("SELECT m.n, n FROM pmsg WHERE id = `a`"),
			http.StatusBadRequest, validation("Duplicate identifiers in select clause: n"),
		},
		{
			"overlap, later path first", opStatement, stmt("SELECT m, m.x FROM pmsg WHERE id = `a`"),
			http.StatusBadRequest, validation(overlap + "path one: [m, x], path two: [m]"),
		},
		{
			"batch key update names its table", opBatchStmt, batch("UPDATE pmsg SET id = `q` WHERE id = `a`"),
			http.StatusOK, `{"Responses":[{"TableName":"pmsg","Error":{"Code":"ValidationError","Message":` +
				`"One or more parameter values were invalid: Cannot update attribute id. This attribute is part of the key"}}]}`,
		},
	})

	require.False(t, s.present(t, "pmsg", "t"), "a refused transaction writes nothing")

	got := s.ok(t, opGet, `{"TableName":"pmsg","Key":{"id":{"S":"a"}}}`)
	require.Equal(t, []any{"1", "5"}, path(t, got, "Item", "nn", "NS"), "only the set parameter was added")
}

// Test_partiql_tokenOtherHash: a NextToken from one hash key, given to a
// Query on another, starts that Query at its beginning when its hash key is
// higher and reads nothing when it is lower (verification/SQ-229 probe6).
func Test_partiql_tokenOtherHash(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, rangeKeyTable("ptok"))

	for _, h := range []string{"g", "h", "k"} {
		for _, sk := range []string{"0", "1", "2"} {
			s.ok(t, opPut, `{"TableName":"ptok","Item":{"id":{"S":"`+h+`"},"sk":{"S":"`+sk+`"}}}`)
		}
	}

	page := s.ok(t, opStatement, stmt("SELECT * FROM ptok WHERE id = `h`", `,"Limit":1`))

	token, ok := page["NextToken"].(string)
	require.True(t, ok, "first page has a NextToken: %v", page)

	higher := s.ok(t, opStatement, stmt("SELECT * FROM ptok WHERE id = `k`", `,"NextToken":"`+token+`"`))
	require.Len(t, higher["Items"], 3, "a higher hash key reads all of its items")

	lower := s.ok(t, opStatement, stmt("SELECT * FROM ptok WHERE id = `g` ORDER BY sk DESC", `,"NextToken":"`+token+`"`))
	require.Empty(t, lower["Items"], "a lower hash key reads none, in either direction")

	// The token's start key, sk 0, is below the range condition sk > 1: an
	// ascending read starts at the condition's first item, a descending one
	// reads nothing (verification/SQ-229 probe6, "tq6 up -> h range").
	above := s.ok(t, opStatement, stmt("SELECT * FROM ptok WHERE id = `h` AND sk > `1`", `,"NextToken":"`+token+`"`))
	require.Len(t, above["Items"], 1, "%v", above)
	require.Equal(t, "2", path(t, above, "Items", 0, "sk", "S"))

	down := s.ok(t, opStatement, stmt("SELECT * FROM ptok WHERE id = `h` AND sk > `1` ORDER BY sk DESC",
		`,"NextToken":"`+token+`"`))
	require.Empty(t, down["Items"], "a descending read from below the condition reads none")
}
