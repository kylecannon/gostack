//nolint:unqueryvet // the statements under test are PartiQL, SELECT * included
package dynamodb_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// What the image holds an expression a PartiQL statement compiles to, besides
// the statement's own 8,192 bytes (SQ-452): the operators of an UpdateExpression
// or a condition, 300, and the 100 operands of an IN. Its size it does not hold;
// a request sent to the item operations is held to 4,096 characters as before.
// The rows are what the LocalStack image (gostack-parity-localstack:8b9a79f)
// answered; limitSteps are the rows themselves, so that a differential can send
// each to the image.

// tooManyOperators is the message the image answers 301 operators with.
const tooManyOperators = "The expression contains too many operators; operator count: 301"

// The bodies the rows answer more than once.
const (
	noResponses = `{"Responses":[]}`
	numberZero  = `{"N":"0"}`
)

// limitCase is a table with the items the rows of a limit test run over, and
// the rows.
type limitCase struct {
	table string
	steps []step
}

// limitTable is a CreateTable body of table limit, keyed by id.
func limitTable(name string) string { return hashTable(name) }

// putSteps are PutItem steps for items, each the JSON of an item.
func putSteps(table string, items ...string) []step {
	steps := make([]step, len(items))
	for i, item := range items {
		steps[i] = step{
			"put", opPut, `{"TableName":"` + table + `","Item":` + item + `}`, http.StatusOK, `{}`,
		}
	}

	return steps
}

// itemIs is a GetItem step that expects item, the JSON of an item, or none.
func itemIs(table, id, item string) step {
	want := `{}`
	if item != "" {
		want = `{"Item":` + item + `}`
	}

	return step{"get " + id, opGet, `{"TableName":"` + table + `","Key":{"id":{"S":"` + id + `"}}}`, http.StatusOK, want}
}

// surface is one way a statement is sent: the call, the body around a
// statement, and the answers to a statement that is applied and to one the
// image refuses with msg.
type surface struct {
	name    string
	op      string
	wrap    func(src string) string
	applied func(table string) (int, string)
	refused func(msg string) (int, string)
}

// surfaces are ExecuteStatement, BatchExecuteStatement and ExecuteTransaction.
func surfaces() []surface {
	return []surface{
		{
			viaStatement, opStatement, func(src string) string { return stmt(src) },
			func(string) (int, string) { return http.StatusOK, `{"Items":[]}` },
			func(msg string) (int, string) { return http.StatusBadRequest, validation(msg) },
		},
		{
			viaBatch, opBatchStmt, func(src string) string { return `{"Statements":[` + stmt(src) + `]}` },
			func(table string) (int, string) {
				return http.StatusOK, `{"Responses":[{"TableName":"` + table + `"}]}`
			},
			func(msg string) (int, string) {
				return http.StatusOK, `{"Responses":[{"Error":{"Code":"ValidationError","Message":` + jsonString(msg) + `}}]}`
			},
		},
		{
			"transaction", opTransaction, func(src string) string { return `{"TransactStatements":[` + stmt(src) + `]}` },
			func(string) (int, string) { return http.StatusOK, noResponses },
			func(msg string) (int, string) {
				return http.StatusBadRequest, validation("Validation failed in TransactStatements[0]: " + msg)
			},
		},
	}
}

// repeat is n terms, term(0) to term(n-1), joined by sep.
func repeat(n int, sep string, term func(i int) string) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = term(i)
	}

	return strings.Join(parts, sep)
}

// operatorItem is the item the operator rows update: id, k0 = 1 and l = [1].
func operatorItem(id string) string {
	return `{"id":{"S":"` + id + `"},"k0":{"N":"1"},"l":{"L":[{"N":"1"}]}}`
}

// operatorKinds are the operators an UPDATE counts and the value each leaves in
// an attribute, for the item operatorItem gives.
var operatorKinds = []struct {
	name   string
	action func(i int) string // PartiQL SET action
	value  string             // the value it leaves
}{
	{"sum", func(i int) string { return "u" + strconv.Itoa(i) + "=k0+1" }, `{"N":"2"}`},
	{"difference", func(i int) string { return "u" + strconv.Itoa(i) + "=k0-1" }, numberZero},
	{
		"list_append", func(i int) string { return "u" + strconv.Itoa(i) + "=list_append(l,[1])" },
		`{"L":[{"N":"1"},{"N":"1"}]}`,
	},
}

// updated is operatorItem with n attributes u0 to u(n-1) of value, as an UPDATE
// of them leaves it.
func updated(id string, n int, value string) string {
	var b strings.Builder

	b.WriteString(strings.TrimSuffix(operatorItem(id), "}"))

	for i := range n {
		b.WriteString(`,"u` + strconv.Itoa(i) + `":` + value)
	}

	b.WriteString("}")

	return b.String()
}

// operatorCountCase is the rows of an UPDATE of n sums, differences or
// list_append calls through each surface: applied at 300 and refused at 301,
// with the item as it was.
func operatorCountCase() limitCase {
	const table = "pops"

	c := limitCase{table: table}

	for _, kind := range operatorKinds {
		for _, sf := range surfaces() {
			for _, n := range []int{300, 301} {
				id := strings.Join([]string{sf.name, kind.name, strconv.Itoa(n)}, "-")
				src := "UPDATE " + table + " SET " + repeat(n, ",", kind.action) + " WHERE id = '" + id + "'"
				status, want := sf.applied(table)
				after := updated(id, n, kind.value)

				if n > 300 {
					status, want = sf.refused(tooManyOperators)
					after = operatorItem(id)
				}

				c.steps = append(c.steps, putSteps(table, operatorItem(id))...)
				c.steps = append(c.steps, step{id, sf.op, sf.wrap(src), status, want}, itemIs(table, id, after))
			}
		}
	}

	return c
}

// Test_partiql_updateOperatorCount: an UPDATE of 300 sums, differences or
// list_append calls is applied and one of 301 is refused with the item as it
// was, through ExecuteStatement, BatchExecuteStatement and ExecuteTransaction,
// as the image answers. The expression such an UPDATE compiles to is past
// 4,096 characters and held to no size, but not to no operators.
func Test_partiql_updateOperatorCount(t *testing.T) {
	c := operatorCountCase()

	s := newStack(t)
	s.ok(t, opCreate, limitTable(c.table))
	run(t, s, c.steps)
}

// directOperatorCase is the rows of an UpdateItem and a TransactWriteItems that
// send 300 and 301 sums, which fit 4,096 characters, and the count of the
// functions: 290 sums with 10 calls are applied and with 11 refused, as the
// image answers. A request has the count the parser gives a statement.
func directOperatorCase() limitCase {
	const table = "pdirect"

	c := limitCase{table: table}
	values := `"ExpressionAttributeValues":{":v":{"N":"1"}}`
	sums := func(n int) string {
		return repeat(n, ",", func(i int) string { return "u" + strconv.Itoa(i) + "=k0+:v" })
	}
	calls := func(n int) string {
		return repeat(n, ",", func(i int) string { return "f" + strconv.Itoa(i) + "=if_not_exists(k0,:v)" })
	}
	update := func(id, expr string) string {
		return `"TableName":"` + table + `","Key":{"id":{"S":"` + id + `"}},"UpdateExpression":` + jsonString(expr) + "," +
			values
	}
	refusal := "Invalid UpdateExpression: " + tooManyOperators

	for _, row := range []struct {
		id, expr string
		refused  bool
	}{
		{"sums300", "SET " + sums(300), false},
		{"sums301", "SET " + sums(301), true},
		{"sums290-calls10", "SET " + sums(290) + "," + calls(10), false},
		{"sums290-calls11", "SET " + sums(290) + "," + calls(11), true},
	} {
		for _, via := range []string{opUpdateItem, opTransactWrite} {
			id := via + "-" + row.id
			body := `{` + update(id, row.expr) + `}`

			if via == opTransactWrite {
				body = `{"TransactItems":[{"Update":{` + update(id, row.expr) + `}}]}`
			}

			c.steps = append(c.steps, putSteps(table, operatorItem(id))...)

			if row.refused {
				c.steps = append(c.steps, step{id, via, body, http.StatusBadRequest, validation(refusal)},
					itemIs(table, id, operatorItem(id)))

				continue
			}

			c.steps = append(c.steps, step{id, via, body, http.StatusOK, `{}`})
		}
	}

	return c
}

// Test_updateItem_operatorCount: the count is the parser's, so an UpdateItem and
// a TransactWriteItems that send it 301 operators get the refusal a statement
// does, and 300 are applied.
func Test_updateItem_operatorCount(t *testing.T) {
	c := directOperatorCase()

	s := newStack(t)
	s.ok(t, opCreate, limitTable(c.table))
	run(t, s, c.steps)
}

// listOf is the PartiQL list [0, 1, ..., n-1].
func listOf(n int) string {
	return "[" + repeat(n, ",", strconv.Itoa) + "]"
}

// inLists is lists conditions of one attribute in a list of 100, joined by OR:
// k0 IN [0..99] OR k1 IN [0..99] ... Each renders to 700 characters and more.
func inLists(lists int) string {
	return repeat(lists, " OR ", func(i int) string { return "k" + strconv.Itoa(i) + " IN " + listOf(100) })
}

// whereItem is an item that holds k0 to k7 as 1, which every IN list of
// inLists has.
func whereItem(id string) string {
	var b strings.Builder

	b.WriteString(`{"id":{"S":"` + id + `"}`)

	for i := range 8 {
		b.WriteString(`,"k` + strconv.Itoa(i) + `":{"N":"1"}`)
	}

	b.WriteString("}")

	return b.String()
}

// whereCase is the rows of a WHERE of 6 and of 8 IN lists, 4,172 and 5,602
// characters once rendered, through every statement that takes one: SELECT as
// a Scan and as a Query, UPDATE and DELETE through each surface, and EXISTS.
func whereCase() limitCase {
	const table = "pwhere"

	c := limitCase{table: table}
	c.steps = putSteps(table, whereItem("a"))

	// The reads come first, while the table holds the one item they find.
	for _, lists := range []int{6, 8} {
		in := inLists(lists)
		n := strconv.Itoa(lists)
		scan := "SELECT id FROM " + table + " WHERE " + in
		query := "SELECT id FROM " + table + " WHERE id = 'a' AND (" + in + ")"
		found := `{"Items":[{"id":{"S":"a"}}]}`

		c.steps = append(c.steps,
			step{"scan " + n, opStatement, stmt(scan), http.StatusOK, found},
			step{"query " + n, opStatement, stmt(query), http.StatusOK, found},
			step{
				"exists " + n, opTransaction,
				`{"TransactStatements":[` + stmt("EXISTS(SELECT * FROM "+table+" WHERE id = 'a' AND ("+in+"))") + `]}`,
				http.StatusOK, noResponses,
			},
		)
	}

	for _, lists := range []int{6, 8} {
		in := inLists(lists)
		n := strconv.Itoa(lists)

		for _, sf := range surfaces() {
			id := "u-" + sf.name + "-" + n
			status, want := sf.applied(table)
			update := "UPDATE " + table + " SET x = 1 WHERE id = '" + id + "' AND (" + in + ")"

			c.steps = append(c.steps, putSteps(table, whereItem(id))...)
			c.steps = append(c.steps, step{"update " + id, sf.op, sf.wrap(update), status, want},
				itemIs(table, id, strings.TrimSuffix(whereItem(id), "}")+`,"x":{"N":"1"}}`))

			id = "d-" + sf.name + "-" + n
			remove := "DELETE FROM " + table + " WHERE id = '" + id + "' AND (" + in + ")"

			c.steps = append(c.steps, putSteps(table, whereItem(id))...)
			c.steps = append(c.steps, step{"delete " + id, sf.op, sf.wrap(remove), status, want}, itemIs(table, id, ""))
		}
	}

	return c
}

// Test_partiql_whereIsNotSized: a WHERE of 6 or 8 IN lists of 100 values, which
// renders past the 4,096 characters of an expression, is answered by every
// statement that takes one, as the image answers it. A SELECT of it is a Scan or
// a Query, an UPDATE and a DELETE an UpdateItem and a DeleteItem with it as the
// condition, an EXISTS a ConditionCheck; each held it to the 4,096 characters
// of a request's expression (SQ-452).
func Test_partiql_whereIsNotSized(t *testing.T) {
	c := whereCase()

	s := newStack(t)
	s.ok(t, opCreate, limitTable(c.table))
	run(t, s, c.steps)
}

// Test_partiql_whereKeepsItsOtherLimits: the operators and the operands of an IN
// of a long WHERE are counted as the image counts them.
func Test_partiql_whereKeepsItsOtherLimits(t *testing.T) {
	const table = "pwhere"

	s := newStack(t)
	s.ok(t, opCreate, limitTable(table))
	run(t, s, []step{
		{
			"operators", opStatement, stmt("SELECT id FROM " + table + " WHERE " +
				repeat(160, " OR ", func(i int) string { return "k" + strconv.Itoa(i) + "=1" })),
			http.StatusBadRequest, validation(tooManyOperators),
		},
		{
			"operators of an update", opStatement, stmt("UPDATE " + table + " SET x = 1 WHERE id = 'a' AND (" +
				repeat(250, " OR ", func(i int) string { return "k" + strconv.Itoa(i) + "=1" }) + ")"),
			http.StatusBadRequest, validation(tooManyOperators),
		},
		{
			"operands", opStatement, stmt("SELECT id FROM " + table + " WHERE k0 IN " + listOf(101)),
			http.StatusBadRequest, validation("The IN operator is provided with too many operands; number of operands: 101"),
		},
	})
}

// longCondition is 10 lists of 100 values, k<i> IN (:a0, ..., :a99), joined by
// OR: a condition past 4,096 characters, as a caller writes it.
func longCondition() (expr, values string) {
	expr = repeat(10, " OR ", func(i int) string {
		return "k" + strconv.Itoa(i) + " IN (" + repeat(100, ",", func(j int) string { return ":a" + strconv.Itoa(j) }) + ")"
	})
	values = `"ExpressionAttributeValues":{` +
		repeat(100, ",", func(j int) string { return `":a` + strconv.Itoa(j) + `":{"N":"1"}` }) + `}`

	return expr, values
}

// directSizeCase is the rows of every call a caller sends that takes an
// expression past 4,096 characters, which the image refuses with the size
// message, and a statement before them changes nothing: ExecuteStatement,
// BatchExecuteStatement and ExecuteTransaction mark only the calls they make
// themselves. Among them are the calls a transaction and a batch are, which
// run in the one place a statement's calls do (SQ-452).
func directSizeCase() limitCase {
	const table = "pdsize"

	c := limitCase{table: table}
	expr, values := longCondition()
	size := strconv.Itoa(len(expr))
	long := func(kind string) string {
		return "Invalid " + kind + ": Expression size has exceeded the maximum allowed size; expression size: " + size
	}
	projection := `"ProjectionExpression":` + jsonString(strings.ReplaceAll(selectList("k", 1000), ",", ", "))
	projMessage := "Invalid ProjectionExpression: Expression size has exceeded the maximum allowed size;"
	key := `"Key":{"id":{"S":"a"}}`
	head := `"TableName":"` + table + `",`
	cond := `"ConditionExpression":` + jsonString(expr) + `,` + values
	actions := repeat(400, ", ", func(i int) string { return "u" + strconv.Itoa(i) + " = :v" })
	update := `"UpdateExpression":` + jsonString("SET "+actions) + `,"ExpressionAttributeValues":{":v":{"N":"1"}}`
	tx := func(action string) string { return `{"TransactItems":[{` + action + `}]}` }
	bad := func(msg string) (int, string) { return http.StatusBadRequest, validation(msg) }

	filter := `"FilterExpression":` + jsonString(expr) + `,`
	item := `"Item":` + whereItem("a") + `,`
	setX := `"UpdateExpression":"SET x = :a0",`
	badCond, badFilter := long("ConditionExpression"), long("FilterExpression")
	badUpdate := "Invalid UpdateExpression: Expression size has exceeded the maximum allowed size;"
	batchGet := `{"RequestItems":{"` + table + `":{"Keys":[{"id":{"S":"a"}}],` + projection + `}}}`

	rows := []struct {
		name, op, body, msg string
	}{
		{"scan filter", opScan, `{` + head + filter + values + `}`, badFilter},
		{
			"query filter", opQuery, `{` + head + `"KeyConditionExpression":"id = :i",` + filter +
				strings.Replace(values, `{`, `{":i":{"S":"a"},`, 1) + `}`, badFilter,
		},
		{"scan projection", opScan, `{` + head + projection + `}`, projMessage},
		{"get projection", opGet, `{` + head + key + `,` + projection + `}`, projMessage},
		{"put condition", opPut, `{` + head + item + cond + `}`, badCond},
		{"delete condition", opDeleteI, `{` + head + key + `,` + cond + `}`, badCond},
		{"update condition", opUpdateItem, `{` + head + key + `,` + setX + cond + `}`, badCond},
		{"update expression", opUpdateItem, `{` + head + key + `,` + update + `}`, badUpdate},
		{"batch get projection", opBatchGet, batchGet, projMessage},
		{"tx get projection", opTransactGet, tx(`"Get":{` + head + key + `,` + projection + `}`), projMessage},
		{"tx put condition", opTransactWrite, tx(`"Put":{` + head + item + cond + `}`), badCond},
		{"tx delete condition", opTransactWrite, tx(`"Delete":{` + head + key + `,` + cond + `}`), badCond},
		{"tx check condition", opTransactWrite, tx(`"ConditionCheck":{` + head + key + `,` + cond + `}`), badCond},
		{"tx update condition", opTransactWrite, tx(`"Update":{` + head + key + `,` + setX + cond + `}`), badCond},
		{"tx update expression", opTransactWrite, tx(`"Update":{` + head + key + `,` + update + `}`), badUpdate},
	}

	c.steps = putSteps(table, whereItem("a"))

	// The statements that come first, and then each row again: whatever a
	// statement lifts is its own.
	statements := []step{
		{
			"where statement", opStatement, stmt("SELECT id FROM " + table + " WHERE " + inLists(8)), http.StatusOK,
			`{"Items":[{"id":{"S":"a"}}]}`,
		},
		{
			"select statement", opStatement, stmt("SELECT " + selectList("k", 700) + " FROM " + table + " WHERE id = 'a'"),
			http.StatusOK, `{"Items":[{"k0":{"N":"1"},"k1":{"N":"1"},"k2":{"N":"1"},"k3":{"N":"1"},` +
				`"k4":{"N":"1"},"k5":{"N":"1"},"k6":{"N":"1"},"k7":{"N":"1"}}]}`,
		},
	}

	for _, round := range [][]step{nil, statements} {
		c.steps = append(c.steps, round...)

		for _, row := range rows {
			status, want := bad(row.msg)
			c.steps = append(c.steps, step{row.name, row.op, row.body, status, want})
		}
	}

	return c
}

// Test_directCalls_keepTheSizeLimit: a request sent to an item operation is held
// to the 4,096 characters of an expression, whatever statements run before it
// and in whatever call: Scan, Query, GetItem, PutItem, UpdateItem, DeleteItem,
// BatchGetItem, TransactGetItems and TransactWriteItems. The answers are the
// image's. A caller that marks every call a statement's, in the one place that
// does it for the calls a transaction and a batch make, fails this test.
func Test_directCalls_keepTheSizeLimit(t *testing.T) {
	c := directSizeCase()

	s := newStack(t)
	s.ok(t, opCreate, limitTable(c.table))
	run(t, s, c.steps)
}
