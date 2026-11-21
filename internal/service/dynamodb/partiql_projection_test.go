//nolint:unqueryvet // the statements under test are PartiQL, SELECT * included
package dynamodb_test

import (
	"net/http"
	"strings"
	"testing"
)

// The rows below are the answers of the LocalStack image (gostack-parity-
// localstack:8b9a79f) to the same requests, probed on 2026-11-21 (evidence
// verification/SQ-439, probe1 to probe5).

const oneC = `{"Count":1,"ScannedCount":1,"Items":[{"c":{"N":"3"}}]}`

const paramMismatch = "Number of parameters in request and statement don't match."

const gsiNotProjected = "One or more parameter values were invalid: Global secondary index "

// projectionItem is an item the indexedTable's indexes project differently:
// g_keys projects id, sk and gk; g_incl adds k0 and k5.
const projectionItem = `{"TableName":"pgsi","Item":{"id":{"S":"a"},"sk":{"S":"x"},"gk":{"S":"g"},"lk":{"S":"l"},` +
	`"k0":{"N":"1"},"k5":{"N":"5"},"c":{"N":"3"},"m":{"M":{"x":{"N":"1"}}}}}`

// Test_globalIndex_refusesUnprojected: a Query, a Scan and a SELECT of a global
// index that does not project every attribute are refused when they ask for an
// attribute it does not hold. AWS's Query and Scan pages say "If you query or
// scan a global secondary index, you can only request attributes that are
// projected into the index. Global secondary index queries cannot fetch
// attributes from the parent table." A local index fetches from the table.
func Test_globalIndex_refusesUnprojected(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, indexedTable("pgsi"))
	s.ok(t, opPut, projectionItem)

	query := func(index, projection string) string {
		return `{"TableName":"pgsi","IndexName":"` + index + `","KeyConditionExpression":"gk = :g",` +
			`"ExpressionAttributeValues":{":g":{"S":"g"}},"ProjectionExpression":"` + projection + `"}`
	}
	scan := func(index, projection string) string {
		return `{"TableName":"pgsi","IndexName":"` + index + `","ProjectionExpression":"` + projection + `"}`
	}
	refused := func(index, names string) string {
		return validation(gsiNotProjected + index + " does not project [" + names + "]")
	}

	run(t, s, []step{
		{"query keys c", opQuery, query("g_keys", "c"), http.StatusBadRequest, refused("g_keys", "c")},
		{"scan keys c", opScan, scan("g_keys", "c"), http.StatusBadRequest, refused("g_keys", "c")},
		{"query keys nested", opQuery, query("g_keys", "m.x"), http.StatusBadRequest, refused("g_keys", "m")},
		{"scan keys list index", opScan, scan("g_keys", "m[0]"), http.StatusBadRequest, refused("g_keys", "m")},
		{"scan keys local key", opScan, scan("g_keys", "id, sk, gk, lk"), http.StatusBadRequest, refused("g_keys", "lk")},
		{"scan keys three", opScan, scan("g_keys", "b, a, c, k0"), http.StatusBadRequest, refused("g_keys", "a, b, c, k0")},
		{"scan include one", opScan, scan("g_incl", "k0, c"), http.StatusBadRequest, refused("g_incl", "c")},
		{
			"query include held", opQuery, query("g_incl", "k0, k5, id"), http.StatusOK,
			`{"Count":1,"ScannedCount":1,"Items":[{"id":{"S":"a"},"k0":{"N":"1"},"k5":{"N":"5"}}]}`,
		},
		{"scan all c", opScan, scan("g_all", "c"), http.StatusOK, oneC},
		{"scan local c", opScan, scan("l_keys", "c"), http.StatusOK, oneC},
		{
			"specific attributes", opScan,
			`{"TableName":"pgsi","IndexName":"g_keys","Select":"SPECIFIC_ATTRIBUTES","ProjectionExpression":"c"}`,
			http.StatusBadRequest, refused("g_keys", "c"),
		},
		{
			"select beats projection", opScan,
			`{"TableName":"pgsi","IndexName":"g_keys","Select":"COUNT","ProjectionExpression":"c"}`,
			http.StatusBadRequest, validation("Cannot specify the ProjectionExpression when choosing to get only the Count"),
		},
		{
			"statement keys c", opStatement, stmt(`SELECT c FROM "pgsi"."g_keys" WHERE gk = 'g'`), http.StatusBadRequest,
			refused("g_keys", "c"),
		},
		{
			"statement scan keys", opStatement, stmt(`SELECT c FROM "pgsi"."g_keys"`), http.StatusBadRequest,
			refused("g_keys", "c"),
		},
		{
			"statement keys two", opStatement, stmt(`SELECT a, c FROM "pgsi"."g_keys" WHERE gk = 'g'`), http.StatusBadRequest,
			refused("g_keys", "a, c"),
		},
		{
			"statement include", opStatement, stmt(`SELECT k0, c FROM "pgsi"."g_incl" WHERE gk = 'g'`), http.StatusBadRequest,
			refused("g_incl", "c"),
		},
		{
			"statement star", opStatement, stmt(`SELECT * FROM "pgsi"."g_keys" WHERE gk = 'g'`), http.StatusOK,
			`{"Items":[{"id":{"S":"a"},"sk":{"S":"x"},"gk":{"S":"g"}}]}`,
		},
		{
			"statement local", opStatement, stmt(`SELECT c FROM "pgsi"."l_keys" WHERE id = 'a'`), http.StatusOK,
			`{"Items":[{"c":{"N":"3"}}]}`,
		},
	})
}

// Test_globalIndex_refusesUnprojectedFilter: a Query's filter that names an
// attribute a global index does not project is refused, its names in order. A
// statement's WHERE is not, as the image answers it from the table's item.
func Test_globalIndex_refusesUnprojectedFilter(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, indexedTable("pgsi"))
	s.ok(t, opPut, projectionItem)

	query := func(index, filter string) string {
		return `{"TableName":"pgsi","IndexName":"` + index + `","KeyConditionExpression":"gk = :g",` +
			`"FilterExpression":"` + filter + `","ExpressionAttributeValues":{":g":{"S":"g"},":v":{"N":"3"}}}`
	}
	refused := func(index, names string) string {
		return validation("One or more parameter values were invalid: Secondary index " + index +
			" does not project one or more filter attributes: [" + names + "]")
	}

	run(t, s, []step{
		{"filter c", opQuery, query("g_keys", "c = :v"), http.StatusBadRequest, refused("g_keys", "c")},
		{"filter held", opQuery, query("g_incl", "k0 = :v"), http.StatusOK, `{"Count":0,"ScannedCount":1,"Items":[]}`},
		{
			"filter names in order", opQuery, query("g_keys", "zz1 = :v AND c = :v AND k0 = :v AND zz2 = :v"),
			http.StatusBadRequest, refused("g_keys", "c, k0, zz1, zz2"),
		},
		{"filter nested", opQuery, query("g_keys", "m.x = :v"), http.StatusBadRequest, refused("g_keys", "m")},
		{
			"filter all", opQuery, query("g_all", "c = :v"), http.StatusOK,
			`{"Count":1,"ScannedCount":1,"Items":[{"id":{"S":"a"},"sk":{"S":"x"},"gk":{"S":"g"},"lk":{"S":"l"},` +
				`"k0":{"N":"1"},"k5":{"N":"5"},"c":{"N":"3"},"m":{"M":{"x":{"N":"1"}}}}]}`,
		},
	})
}

// Test_partiql_batchStatementTexts: a statement of a BatchExecuteStatement over
// 8,192 bytes or empty is refused for the whole call, wherever it stands and
// before any statement's syntax; where it stands among the other whole-call
// refusals is Test_partiql_batchOrder's (SQ-439 probe3, probe4).
func Test_partiql_batchStatementTexts(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("pbt"))
	s.ok(t, opPut, `{"TableName":"pbt","Item":{"id":{"S":"a"}}}`)

	ok := "SELECT * FROM pbt WHERE id = `a`"
	long := ok + strings.Repeat(" ", 8193-len(ok))
	exact := ok + strings.Repeat(" ", 8192-len(ok))
	tooLong := validation("Member must have length less than or equal to 8192")
	empty := validation("The statement was empty.")

	run(t, s, []step{
		{"long alone", opBatchStmt, batch(long), http.StatusBadRequest, tooLong},
		{"long second", opBatchStmt, batch(ok, long), http.StatusBadRequest, tooLong},
		{"long after syntax", opBatchStmt, batch("SELEC", long), http.StatusBadRequest, tooLong},
		{"long before empty", opBatchStmt, batch(long, ""), http.StatusBadRequest, tooLong},
		{"empty before long", opBatchStmt, batch("", long), http.StatusBadRequest, empty},
		{"empty second", opBatchStmt, batch(ok, ""), http.StatusBadRequest, empty},
		{"exact", opBatchStmt, batch(exact), http.StatusOK, `{"Responses":[{"TableName":"pbt","Item":{"id":{"S":"a"}}}]}`},
	})
}

// Test_partiql_transactionMessages: ExecuteTransaction's refusals of an empty
// statement, which does not name it, and of a parameter-count mismatch, which
// names the first statement after every statement has parsed (SQ-439 probe3,
// probe4).
func Test_partiql_transactionMessages(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("ptm"))

	ok := "SELECT * FROM ptm WHERE id = `a`"
	withParam := "SELECT * FROM ptm WHERE id = ?"
	emptyTx := validation("ExecutePartiQLTransaction can not have empty statement.")
	mismatch := func(i string) string {
		return validation("Validation failed in TransactStatements[" + i + "]: " + paramMismatch)
	}

	run(t, s, []step{
		{"empty statement", opTransaction, transaction(""), http.StatusBadRequest, emptyTx},
		{"empty second", opTransaction, transaction(ok, ""), http.StatusBadRequest, emptyTx},
		{"missing", opTransaction, `{"TransactStatements":[{}]}`, http.StatusBadRequest, emptyTx},
		{"empty after parameter", opTransaction, transaction(withParam, ""), http.StatusBadRequest, emptyTx},
		{"parameter first", opTransaction, transaction(withParam), http.StatusBadRequest, mismatch("0")},
		{"parameter second", opTransaction, transaction(ok, withParam), http.StatusBadRequest, mismatch("1")},
		{
			"parameter short", opTransaction, transaction("SELECT * FROM ptm WHERE id = ? AND p = ?"),
			http.StatusBadRequest, mismatch("0"),
		},
	})
}

// Test_partiql_nestedPathShapes: a SELECT of a path below the top level. A
// batch and a transaction answer the item as a GetItem keeps it; an
// ExecuteStatement keys each path by its last name, or, for a path that goes
// through a list element past the first, by that element, which it answers
// with what the path asks of it (SQ-439 probe3, probe4, probe5).
func Test_partiql_nestedPathShapes(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("pnest"))
	s.ok(t, opPut, `{"TableName":"pnest","Item":{"id":{"S":"a"},`+
		`"m":{"M":{"x":{"M":{"w":{"N":"1"},"v":{"N":"2"}}},"q":{"N":"5"}}},`+
		`"l":{"L":[{"N":"1"},{"L":[{"N":"10"},{"N":"20"}]},{"M":{"z":{"M":{"w":{"N":"7"}}},"y":{"N":"8"}}}]}}}`)

	items := func(body string) string { return `{"Items":[` + body + `]}` }
	single := func(sql string) string { return stmt(sql + " FROM pnest WHERE id = `a`") }

	run(t, s, []step{
		{"statement last name", opStatement, single("SELECT m.x.w"), http.StatusOK, items(`{"w":{"N":"1"}}`)},
		{"statement first element", opStatement, single("SELECT l[0]"), http.StatusOK, items(`{"l[0]":{"N":"1"}}`)},
		{
			"statement element", opStatement, single("SELECT l[2]"), http.StatusOK,
			items(`{"l[2]":{"M":{"z":{"M":{"w":{"N":"7"}}},"y":{"N":"8"}}}}`),
		},
		{
			"statement below element", opStatement, single("SELECT l[2].z"), http.StatusOK,
			items(`{"l[2]":{"M":{"z":{"M":{"w":{"N":"7"}}}}}}`),
		},
		{
			"statement below element twice", opStatement, single("SELECT l[2].z.w"), http.StatusOK,
			items(`{"l[2]":{"M":{"z":{"M":{"w":{"N":"7"}}}}}}`),
		},
		{
			"statement two below one element", opStatement, single("SELECT l[2].z, l[2].y"), http.StatusOK,
			items(`{"l[2]":{"M":{"z":{"M":{"w":{"N":"7"}}},"y":{"N":"8"}}}}`),
		},
		{
			"statement list in list", opStatement, single("SELECT l[1][0]"), http.StatusOK,
			items(`{"l[1]":{"L":[{"N":"10"}]}}`),
		},
		{
			"statement list in list both", opStatement, single("SELECT l[1][0], l[1][1]"), http.StatusOK,
			items(`{"l[1]":{"L":[{"N":"10"},{"N":"20"}]}}`),
		},
		{"statement missing", opStatement, single("SELECT l[2].nope"), http.StatusOK, items(`{}`)},
		{
			"statement scan", opStatement, stmt("SELECT l[2].z, m.q FROM pnest"), http.StatusOK,
			items(`{"l[2]":{"M":{"z":{"M":{"w":{"N":"7"}}}}},"q":{"N":"5"}}`),
		},
		{
			"batch two paths", opBatchStmt, batch("SELECT m.x.w, l[2].z FROM pnest WHERE id = `a`"), http.StatusOK,
			`{"Responses":[{"TableName":"pnest","Item":{"m":{"M":{"x":{"M":{"w":{"N":"1"}}}}},` +
				`"l":{"L":[{"M":{"z":{"M":{"w":{"N":"7"}}}}}]}}}]}`,
		},
		{
			"transaction two paths", opTransaction, transaction("SELECT m.q, l[1][0] FROM pnest WHERE id = `a`"), http.StatusOK,
			`{"Responses":[{"Item":{"m":{"M":{"q":{"N":"5"}}},"l":{"L":[{"L":[{"N":"10"}]}]}}}]}`,
		},
	})
}

// Test_partiql_batchOrder: the checks a BatchExecuteStatement makes of the call
// go statement by statement in order. An empty or long statement refuses the
// call where it stands, and so does one that mixes reads and writes with those
// before it, where a statement that does not parse has no kind, and one whose
// parameters are short has its kind; then the count. The rows are the image's
// answers (verification/SQ-467 image-probe.log, SQ-439 probe).
func Test_partiql_batchOrder(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("pbo"))

	const (
		read    = "SELECT * FROM pbo WHERE id = `a`"
		write   = "UPDATE pbo SET q = 1 WHERE id = `w5`"
		short   = "UPDATE pbo SET q = ? WHERE id = `w1`"
		garbage = "garbage in"
	)

	long := read + strings.Repeat(" ", 8193-len(read))
	mixed := validation("Read and write requests together in the same batch is not supported.")
	empty := validation("The statement was empty.")
	tooLong := validation("Member must have length less than or equal to 8192")
	manyReads := validation("BatchGetItem cannot fetch more than 25 items")
	manyWrites := validation("BatchWriteItem cannot write more than 25 items")

	repeat := func(src string, n int) []string {
		return strings.Split(strings.TrimSuffix(strings.Repeat(src+"\n", n), "\n"), "\n")
	}

	for _, c := range []struct {
		name string
		srcs []string
		want string
	}{
		{"read write empty", []string{read, write, ""}, mixed},
		{"read write long", []string{read, write, long}, mixed},
		{"read write garbage", []string{read, write, garbage}, mixed},
		{"read garbage write", []string{read, garbage, write}, mixed},
		{"read short", []string{read, short}, mixed},
		{"short write read", []string{short, write, read}, mixed},
		{"read empty write", []string{read, "", write}, empty},
		{"empty read write", []string{"", read, write}, empty},
		{"long read write", []string{long, read, write}, tooLong},
		{"empty before later mixed and long", []string{"", read, write, long}, empty},
		{"26 mixed", append(repeat(read, 13), repeat(write, 13)...), mixed},
		{"26 reads short", append(repeat(read, 25), short), mixed},
		{"26 reads garbage", append(repeat(read, 25), garbage), manyReads},
		{"26 garbage", repeat(garbage, 26), manyWrites},
		{"26 writes", repeat(write, 26), manyWrites},
		{"26 reads long", append(repeat(read, 25), long), tooLong},
	} {
		run(t, s, []step{{c.name, opBatchStmt, batch(c.srcs...), http.StatusBadRequest, c.want}})
	}
}

// Test_partiql_transactionOrder: the checks an ExecuteTransaction makes go
// statement by statement in order too: an empty or long statement, a statement
// that does not parse, and one that mixes reads and writes with those before
// it refuse the call where it stands. A parameter mismatch and a RETURNING
// clause wait for after that, and then come in order with the statement's table
// and the rest of what is wrong with it. The rows are the image's answers
// (verification/SQ-467 image-probe.log, SQ-439 probe).
func Test_partiql_transactionOrder(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("pto"))

	const (
		read      = "SELECT * FROM pto WHERE id = `a`"
		write     = "UPDATE pto SET q = 1 WHERE id = `w5`"
		short     = "UPDATE pto SET q = ? WHERE id = `w1`"
		returning = "UPDATE pto SET q = 1 WHERE id = `w3` RETURNING ALL NEW *"
		missing   = "UPDATE nope SET q = 1 WHERE id = `w`"
		exists    = "EXISTS(SELECT * FROM pto WHERE id = `a`)"
		garbage   = "garbage in"
	)

	long := read + strings.Repeat(" ", 8193-len(read))
	mixed := validation("ExecuteTransaction API does not support both read and write operations in the same request.")
	empty := validation("ExecutePartiQLTransaction can not have empty statement.")
	tooLong := validation("Member must have length less than or equal to 8192")
	syntax := validation("Statement wasn't well formed, can't be processed: " + garbage)
	failed := func(i, msg string) string {
		return validation("Validation failed in TransactStatements[" + i + "]: " + msg)
	}
	mismatch := func(i string) string { return failed(i, paramMismatch) }
	returnsAt := func(i string) string { return failed(i, "RETURNING clause is not supported in ExecuteTransaction.") }
	notFound := `{"__type":"ResourceNotFoundException","message":"Requested resource not found"}`

	for _, c := range []struct {
		name string
		srcs []string
		want string
	}{
		{"read write empty", []string{read, write, ""}, mixed},
		{"read write long", []string{read, write, long}, mixed},
		{"read write garbage", []string{read, write, garbage}, mixed},
		{"read garbage write", []string{read, garbage, write}, syntax},
		{"read empty write", []string{read, "", write}, empty},
		{"long read write", []string{long, read, write}, tooLong},
		{"garbage long", []string{garbage, long}, syntax},
		{"long garbage", []string{long, garbage}, tooLong},
		{"garbage empty", []string{garbage, ""}, syntax},
		{"empty garbage", []string{"", garbage}, empty},
		{"read short", []string{read, short}, mixed},
		{"short read", []string{short, read}, mixed},
		{"read returning", []string{read, returning}, mixed},
		{"returning read", []string{returning, read}, mixed},
		{"write returning read", []string{write, returning, read}, mixed},
		{"exists read garbage", []string{exists, read, garbage}, mixed},
		{"short returning", []string{short, returning}, mismatch("0")},
		{"returning short", []string{returning, short}, returnsAt("0")},
		{"write returning short", []string{write, returning, short}, returnsAt("1")},
		{"returning garbage", []string{returning, garbage}, syntax},
		{"short returning garbage", []string{short, returning, garbage}, syntax},
		{"missing short", []string{missing, short}, notFound},
		{"short missing", []string{short, missing}, mismatch("0")},
	} {
		run(t, s, []step{{c.name, opTransaction, transaction(c.srcs...), http.StatusBadRequest, c.want}})
	}
}

// Test_partiql_existsOutsideTransaction: an EXISTS statement is refused in
// the image's words by ExecuteStatement and by a batch, which answers it for
// the statement, and it is no kind for the batch's mixing check.
func Test_partiql_existsOutsideTransaction(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("pex"))

	exists := "EXISTS(SELECT * FROM pex WHERE id = `a`)"
	msg := "EXISTS can only be used in ExecuteTransaction write requests."

	run(t, s, []step{
		{"statement", opStatement, stmt(exists), http.StatusBadRequest, validation(msg)},
		{
			viaBatch, opBatchStmt, batch(exists, "UPDATE pex SET q = 1 WHERE id = `w`"), http.StatusOK,
			`{"Responses":[{"Error":{"Code":"ValidationError","Message":"` + msg + `"}},` +
				`{"Error":{"Code":"ConditionalCheckFailed","Message":"The conditional request failed"},"TableName":"pex"}]}`,
		},
	})
}

// Test_partiql_cutPathsDistinctLists: two paths of a SELECT through the same
// index of different lists take one key, `l[1]`, which holds one value. The
// image answers the first path in name order, whole, and never a value of a
// path the statement does not name; the rows are its answers (verification/
// SQ-467 image-probe.log).
func Test_partiql_cutPathsDistinctLists(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("pcut"))
	s.ok(t, opPut, `{"TableName":"pcut","Item":{"id":{"S":"a"},`+
		`"m":{"M":{"l":{"L":[{"M":{"p":{"S":"m0p"}}},{"M":{"p":{"S":"m1p"},"q":{"S":"m1q"}}},`+
		`{"M":{"p":{"S":"m2p"},"q":{"S":"m2q"}}}]}}},`+
		`"n":{"M":{"l":{"L":[{"M":{"p":{"S":"n0p"}}},{"M":{"p":{"S":"n1p"},"q":{"S":"n1q"}}}]}}},`+
		`"o":{"M":{"l":{"L":[{"M":{"x":{"S":"o0x"}}},{"M":{"x":{"S":"o1x"},"y":{"S":"o1y"}}}]}}},`+
		`"p":{"M":{"l":{"L":[{"M":{"x":{"S":"p0x"}}},{"M":{"x":{"S":"p1x"},"y":{"S":"p1y"}}}]}}},`+
		`"l":{"L":[{"M":{"p":{"S":"l0p"}}},{"M":{"p":{"S":"l1p"},"q":{"S":"l1q"},"z":{"S":"l1z"}}}]}}}`)

	element := func(fields string) string { return `{"Items":[{"l[1]":{"M":{` + fields + `}}}]}` }
	m1p, l1p := `"p":{"S":"m1p"}`, `"p":{"S":"l1p"}`

	for _, c := range []struct{ sel, want string }{
		{"m.l[1].p, n.l[1].q", element(m1p)},
		{"n.l[1].q, m.l[1].p", element(m1p)},
		{"l[1].p, m.l[1].q", element(l1p)},
		{"m.l[1].q, l[1].p", element(l1p)},
		{"l[1].p, m.l[1]", element(l1p)},
		{"m.l[1], l[1].p", element(l1p)},
		{"l[1], m.l[1].p", element(l1p + `,"q":{"S":"l1q"},"z":{"S":"l1z"}`)},
		{"m.l[2].p, m.l[2].q", `{"Items":[{"l[2]":{"M":{"p":{"S":"m2p"},"q":{"S":"m2q"}}}}]}`},
		// Name order, not the order of a Java hash set (it holds "p" before
		// "o"), and the next path when the first holds no such element.
		{"o.l[1].x, p.l[1].y", element(`"x":{"S":"o1x"}`)},
		{"p.l[1].y, o.l[1].x", element(`"x":{"S":"o1x"}`)},
		{"o.l[5].x, p.l[1].y", element(`"y":{"S":"p1y"}`)},
		{"p.l[1].y, o.l[5].x", element(`"y":{"S":"p1y"}`)},
	} {
		for _, where := range []string{" FROM pcut", " FROM pcut WHERE id = `a`"} {
			run(t, s, []step{{c.sel + where, opStatement, stmt("SELECT " + c.sel + where), http.StatusOK, c.want}})
		}
	}
}
