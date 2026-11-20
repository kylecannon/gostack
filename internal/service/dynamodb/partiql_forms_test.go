//nolint:unqueryvet // the statements under test are PartiQL, SELECT * included
package dynamodb_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// Test_partiql_readsWholeItem: a SELECT * run through BatchExecuteStatement
// or ExecuteTransaction answers the whole item, a projection the paths it
// names (verification/SQ-229 probe2, "batch capacity" and "tx reads").
func Test_partiql_readsWholeItem(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("pwhole"))
	s.ok(t, opPut, `{"TableName":"pwhole","Item":{"id":{"S":"a"},"n":{"N":"1"},"m":{"M":{"k":{"S":"v"}}}}}`)

	const whole = `{"id":{"S":"a"},"n":{"N":"1"},"m":{"M":{"k":{"S":"v"}}}}`

	run(t, s, []step{
		{
			"batch star", opBatchStmt, `{"Statements":[{"Statement":"SELECT * FROM pwhole WHERE id = 'a'"}]}`,
			http.StatusOK, `{"Responses":[{"TableName":"pwhole","Item":` + whole + `}]}`,
		},
		{
			"batch projection", opBatchStmt, `{"Statements":[{"Statement":"SELECT n, m.k FROM pwhole WHERE id = 'a'"}]}`,
			http.StatusOK, `{"Responses":[{"TableName":"pwhole","Item":{"n":{"N":"1"},"k":{"S":"v"}}}]}`,
		},
		{
			"transaction star", opTransaction, `{"TransactStatements":[{"Statement":"SELECT * FROM pwhole WHERE id = 'a'"},` +
				`{"Statement":"SELECT id FROM pwhole WHERE id = 'zz'"}]}`,
			http.StatusOK, `{"Responses":[{"Item":` + whole + `},{}]}`,
		},
	})
}

// Test_partiql_errors: the messages the LocalStack image answers where a
// statement's own words differ from the operation it compiles to
// (verification/SQ-229 probes 2 to 4).
func Test_partiql_errors(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("perr"))
	s.ok(t, opPut, `{"TableName":"perr","Item":{"id":{"S":"a"},"n":{"N":"1"}}}`)

	run(t, s, []step{
		{
			"limit 0", opStatement, stmt("SELECT * FROM perr", `,"Limit":0`), http.StatusBadRequest,
			validation("1 validation error detected: Value '0' at 'limit' failed to satisfy constraint: " +
				"Member must have value greater than or equal to 1"),
		},
		{
			"parameter of two types", opStatement, stmt("SELECT * FROM perr WHERE id = ?", `,"Parameters":[{"S":"x","N":"1"}]`),
			http.StatusBadRequest, validation("Supplied AttributeValue has more than one datatypes set, " +
				"must contain exactly one of the supported datatypes"),
		},
		{
			"short table name", opStatement, stmt("UPDATE ab SET x = 1 WHERE id = `c`"), http.StatusBadRequest,
			validation("Table name should be within the length [3, 255] and only contain 'a-z', 'A-Z', '0-9', '-', '_', '.'."),
		},
		{
			"repeated string in a bag", opStatement, stmt("INSERT INTO perr VALUE {`id`: `s`, `s`: <<`a`, `a`>>}"),
			http.StatusBadRequest, validation("Input collection contains duplicates"),
		},
		{
			"update overlap has no expression prefix", opStatement, stmt("UPDATE perr SET y = 1, y = 2 WHERE id = `a`"),
			http.StatusBadRequest, validation("Two document paths overlap with each other; must remove or rewrite one " +
				"of these paths; path one: [y], path two: [y]"),
		},
		{
			"batch unknown function before single select", opBatchStmt,
			`{"Statements":[{"Statement":"SELECT * FROM perr WHERE id = 'a' AND foo(n)"}]}`, http.StatusOK,
			`{"Responses":[{"Error":{"Code":"ValidationError","Message":"Unrecognized function: foo"}}]}`,
		},
		{
			"batch filter is not a single select", opBatchStmt,
			`{"Statements":[{"Statement":"SELECT * FROM perr WHERE id = 'a' AND n = 2"}]}`, http.StatusOK,
			`{"Responses":[{"Error":{"Code":"ValidationError","Message":"Only single item select is supported"}}]}`,
		},
		{
			"transaction lone name", opTransaction,
			`{"TransactStatements":[{"Statement":"INSERT INTO perr VALUE {'id': 'ti'}"},` +
				`{"Statement":"SELEC"}]}`, http.StatusBadRequest, validation("Unsupported operation: VariableReference"),
		},
		{
			"failed EXISTS returns no item", opTransaction, `{"TransactStatements":[` +
				`{"Statement":"EXISTS(SELECT * FROM perr WHERE id = 'a' AND n = 5)",` +
				`"ReturnValuesOnConditionCheckFailure":"ALL_OLD"},` +
				`{"Statement":"INSERT INTO perr VALUE {'id': 'tf'}"}]}`, http.StatusBadRequest,
			`{"__type":"TransactionCanceledException","Message":"Transaction cancelled, please refer cancellation reasons ` +
				`for specific reasons [ConditionalCheckFailed, None]","CancellationReasons":[{"Code":"ConditionalCheckFailed",` +
				`"Message":"The conditional request failed."},{"Code":"None"}]}`,
		},
		{
			"token first use", opTransaction, `{"ClientRequestToken":"tok1","TransactStatements":[` +
				`{"Statement":"INSERT INTO perr VALUE {'id': 'tl'}"}]}`, http.StatusOK, `{"Responses":[]}`,
		},
		{
			"token replay", opTransaction, `{"ClientRequestToken":"tok1","TransactStatements":[` +
				`{"Statement":"INSERT INTO perr VALUE {'id': 'tl'}"}]}`, http.StatusOK, `{"Responses":[]}`,
		},
		{
			"token mismatch", opTransaction, `{"ClientRequestToken":"tok1","TransactStatements":[` +
				`{"Statement":"INSERT INTO perr VALUE {'id': 'tm'}"}]}`, http.StatusBadRequest,
			`{"__type":"IdempotentParameterMismatchException","Message":"Specified idempotent token was used with ` +
				`different request parameters within the idempotency window"}`,
		},
	})
}

// Test_partiql_rangePlan: a SELECT that pins the hash key runs as a Query, in
// range key order, bounded by its range condition, and refuses range values
// of the wrong type as DynamoDB Local does (verification/SQ-229 probes 3, 4).
func Test_partiql_rangePlan(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, rangeKeyTable("prange"))

	for _, sk := range []string{"0", "1", "2", "3", "4"} {
		s.ok(t, opPut, `{"TableName":"prange","Item":{"id":{"S":"h"},"sk":{"S":"`+sk+`"}}}`)
	}

	s.ok(t, opPut, `{"TableName":"prange","Item":{"id":{"S":"g"},"sk":{"S":"0"}}}`)

	item := func(sk string) string { return `{"id":{"S":"h"},"sk":{"S":"` + sk + `"}}` }

	run(t, s, []step{
		{
			"hash only, ascending", opStatement, stmt("SELECT * FROM prange WHERE id = `h`"), http.StatusOK,
			`{"Items":[` + item("0") + `,` + item("1") + `,` + item("2") + `,` + item("3") + `,` + item("4") + `]}`,
		},
		{
			"descending", opStatement, stmt("SELECT * FROM prange WHERE id = `h` ORDER BY sk DESC"), http.StatusOK,
			`{"Items":[` + item("4") + `,` + item("3") + `,` + item("2") + `,` + item("1") + `,` + item("0") + `]}`,
		},
		{
			"two bounds", opStatement, stmt("SELECT * FROM prange WHERE id = `h` AND sk > `1` AND sk < `4`"), http.StatusOK,
			`{"Items":[` + item("2") + `,` + item("3") + `]}`,
		},
		{
			"between", opStatement, stmt("SELECT * FROM prange WHERE id = `h` AND sk BETWEEN `1` AND `2`"), http.StatusOK,
			`{"Items":[` + item("1") + `,` + item("2") + `]}`,
		},
		{
			"between numbers", opStatement, stmt("SELECT * FROM prange WHERE id = `h` AND sk BETWEEN 1 AND 3"),
			http.StatusBadRequest, validation("Key attribute's data type should match its data type in table's schema: Key sk"),
		},
		{
			"in numbers", opStatement, stmt("SELECT * FROM prange WHERE id = `h` AND sk IN [1, 3]"), http.StatusBadRequest,
			validation("One or more parameter values were invalid: Condition parameter type does not match schema type"),
		},
		{
			"begins_with a number", opStatement, stmt("SELECT * FROM prange WHERE id = `h` AND begins_with(sk, 1)"),
			http.StatusBadRequest, validation("Incorrect operand type for operator or function; operator or function: " +
				"begins_with, operand type: N"),
		},
		{
			"order by without where", opStatement, stmt("SELECT * FROM prange ORDER BY sk"), http.StatusBadRequest,
			validation("Must have WHERE clause in the statement when using ORDER BY clause."),
		},
		{
			"partial key update", opStatement, stmt("UPDATE prange SET x = 1 WHERE id = `h`"), http.StatusBadRequest,
			validation("Where clause does not contain a mandatory equality on all key attributes"),
		},
	})

	page := s.ok(t, opStatement, stmt("SELECT * FROM prange WHERE id = `h`", `,"Limit":2`))
	require.Len(t, page["Items"], 2)
	require.Equal(t, "1", path(t, page, "Items", 1, "sk", "S"))

	token, ok := page["NextToken"].(string)
	require.True(t, ok, "first page has a NextToken: %v", page)

	next := s.ok(t, opStatement, stmt("SELECT * FROM prange WHERE id = `h`", `,"Limit":2,"NextToken":"`+token+`"`))
	require.Equal(t, "2", path(t, next, "Items", 0, "sk", "S"))
	require.Equal(t, "3", path(t, next, "Items", 1, "sk", "S"))

	s.fails(t, opStatement, stmt("SELECT * FROM prange", `,"NextToken":"!!"`), "ValidationException", "Invalid NextToken")
}
