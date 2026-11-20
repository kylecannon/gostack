package dynamodb_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	opStatement   = "ExecuteStatement"
	opBatchStmt   = "BatchExecuteStatement"
	opTransaction = "ExecuteTransaction"
)

// step is one call of a scripted test and the whole body it answers.
type step struct {
	name   string
	op     string
	body   string
	status int
	want   string
}

// run sends each step in order and compares its whole body.
func run(t *testing.T, s *stack, steps []step) {
	t.Helper()

	for _, st := range steps {
		got := s.call(t, usEast1, st.op, st.body)
		require.Equal(t, st.status, got.status, "%s: %s", st.name, got.body)
		require.JSONEq(t, st.want, got.body, st.name)
	}
}

// stmt is an ExecuteStatement body for the statement src, whose single
// quotes are written as backquotes so the cases read without escapes.
func stmt(src string, extra ...string) string {
	b := jsonString(strings.ReplaceAll(src, "`", "'"))

	return `{"Statement":` + b + strings.Join(extra, "") + `}`
}

func jsonString(s string) string {
	var b strings.Builder

	b.WriteByte('"')

	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}

	b.WriteByte('"')

	return b.String()
}

// validation is the body of a ValidationException with msg.
func validation(msg string) string {
	q := jsonString(msg)

	return `{"__type":"ValidationException","message":` + q + `}`
}

// Test_executeStatement_rows: the calls test_dynamodb_partiql_missing and
// test_dynamodb_execute_statement_empy_parameter make, with the bodies their
// snapshots pin.
func Test_executeStatement_rows(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("pmiss"))

	run(t, s, []step{
		{
			"insert", opStatement, stmt("INSERT INTO pmiss VALUE {`id`: `Alice123`, `FirstName`:`Alice`}"), http.StatusOK,
			noItems,
		},
		{
			"not missing", opStatement, stmt("SELECT id, FirstName FROM pmiss WHERE FirstName IS NOT MISSING"), http.StatusOK,
			`{"Items":[{"FirstName":{"S":"Alice"},"id":{"S":"Alice123"}}]}`,
		},
		{
			"missing", opStatement, stmt("SELECT id, FirstName FROM pmiss WHERE FirstName IS MISSING"),
			http.StatusOK, noItems,
		},
		{
			"empty parameters", opStatement, stmt("SELECT id FROM pmiss", `,"Parameters":[]`), http.StatusBadRequest,
			validation("1 validation error detected: Value '[]' at 'parameters' failed to satisfy constraint: " +
				"Member must have length greater than or equal to 1"),
		},
	})
}

// Test_batchExecuteStatement_row: test_dynamodb_batch_execute_statement's
// insert and update, answered by table name only, and their effect.
func Test_batchExecuteStatement_row(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("pbatch"))
	s.ok(t, opPut, `{"TableName":"pbatch","Item":{"id":{"S":"user02"}}}`)

	run(t, s, []step{
		{
			"batch", opBatchStmt, `{"Statements":[{"Statement":"INSERT INTO pbatch VALUE {'id': 'user01'}"},` +
				`{"Statement":"UPDATE pbatch SET Age=20 WHERE id='user02'"}]}`, http.StatusOK,
			`{"Responses":[{"TableName":"pbatch"},{"TableName":"pbatch"}]}`,
		},
		{
			"user02", opGet, `{"TableName":"pbatch","Key":{"id":{"S":"user02"}}}`, http.StatusOK,
			`{"Item":{"Age":{"N":"20"},"id":{"S":"user02"}}}`,
		},
		{
			"user01", opGet, `{"TableName":"pbatch","Key":{"id":{"S":"user01"}}}`, http.StatusOK,
			`{"Item":{"id":{"S":"user01"}}}`,
		},
	})
}

// Test_executeTransaction_row: test_dynamodb_execute_transaction's two
// inserts answer an empty Responses list.
func Test_executeTransaction_row(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("ptx"))

	got := s.call(t, usEast1, opTransaction, `{"TransactStatements":[`+
		`{"Statement":"INSERT INTO ptx VALUE {'id': 'user01'}"},{"Statement":"INSERT INTO ptx VALUE {'id': 'user02'}"}]}`)
	require.Equal(t, http.StatusOK, got.status, got.body)
	require.JSONEq(t, `{"Responses":[]}`, got.body)

	out := s.ok(t, opScan, `{"TableName":"ptx"}`)
	require.InDelta(t, 2, out["Count"], 0)
}
