//nolint:unqueryvet // the statements under test are PartiQL
package dynamodb_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// What the image applies in the value of a PartiQL UPDATE action, and how it
// counts the operators of a statement's WHERE (SQ-454). The rows are what the
// LocalStack image (gostack-parity-localstack:8b9a79f) answered, probed
// against it, and each is a limitCase's steps, so that a differential can send
// them to the image.

// chainItem is the item the value rows update: id, k0 = 5, l = [1] and s = "x".
func chainItem(id string) string {
	return `{"id":{"S":"` + id + `"},"k0":{"N":"5"},"l":{"L":[{"N":"1"}]},"s":{"S":"x"}}`
}

// withAttrs is item with the members attrs, each `"name":value`, added.
func withAttrs(item string, attrs ...string) string {
	return strings.TrimSuffix(item, "}") + "," + strings.Join(attrs, ",") + "}"
}

const (
	oneOne     = `{"L":[{"N":"1"},{"N":"1"}]}`
	notAList   = "Incorrect operand type for operator or function; operator or function: "
	wrongType  = "An operand in the update expression has an incorrect data type"
	missingRef = "The provided expression refers to an attribute that does not exist in the item"
	needsPath  = "Operator or function requires a document path; operator or function: if_not_exists"

	// numbersInList is how the image names the type of a sum in a list_append.
	numbersInList = "list_append, operand type: {INT,N,FLOAT,HD,DOUBLE,DECIMAL}"
)

// number is the JSON of the number n.
func number(n string) string { return `{"N":"` + n + `"}` }

// listOfNumbers is the JSON of the list of the numbers ns.
func listOfNumbers(ns ...string) string {
	items := make([]string, len(ns))
	for i, n := range ns {
		items[i] = `{"N":"` + n + `"}`
	}

	return `{"L":[` + strings.Join(items, ",") + `]}`
}

// updateValueCase is the rows of an UPDATE whose value a SET action builds from
// sums, differences and functions the UpdateExpression grammar has no place
// for: what the image leaves in the attribute, or the refusal and the item as it
// was.
func updateValueCase() limitCase {
	const table = "pvalue"

	num := func(n int) string { return number(strconv.Itoa(n)) }

	c := limitCase{table: table}

	applied := []struct{ name, set, a string }{
		{"chained sum", "a = k0 + 1 + 1", num(7)},
		{"sum then difference", "a = k0 - 1 + 1", num(5)},
		{"chained difference", "a = k0 - 1 - 1", num(3)},
		{"three sums", "a = k0 + 1 + 1 + 1", num(8)},
		{"parenthesized left", "a = (k0 + 1) + 1", num(7)},
		{"parenthesized right", "a = k0 + (1 + 1)", num(7)},
		{"difference of a sum", "a = k0 - (1 + 1)", num(3)},
		{"both parenthesized", "a = (k0 + 1) + (1 + 1)", num(8)},
		{"parenthesized value", "a = (k0 + 1)", num(6)},
		{"doubly parenthesized value", "a = ((k0 + 1))", num(6)},
		{"value first", "a = 1 + k0 + 1", num(7)},
		{"if_not_exists in a sum", "a = if_not_exists(zz, 1) + 1", num(2)},
		{"if_not_exists after a sum", "a = 1 + if_not_exists(zz, 1)", num(2)},
		{"if_not_exists in a chain", "a = if_not_exists(zz, 1) + 1 + 1", num(3)},
		{"sum in if_not_exists", "a = if_not_exists(zz, k0 + 1)", num(6)},
		{"if_not_exists in if_not_exists", "a = if_not_exists(zz, if_not_exists(yy, 1))", num(1)},
		{"list_append first in list_append", "a = list_append(list_append(l, [1]), [2])", listOfNumbers("1", "1", "2")},
		{"list_append second in list_append", "a = list_append(l,list_append(l,[2]))", listOfNumbers("1", "1", "2")},
		{
			"list_append three deep", "a = list_append(list_append(list_append(l, [1]), [2]), [3])",
			listOfNumbers("1", "1", "2", "3"),
		},
		{"list_append in if_not_exists", "a = if_not_exists(zz, list_append(l, [1]))", oneOne},
		{"if_not_exists in list_append", "a = list_append(if_not_exists(zz, [9]), [1])", listOfNumbers("9", "1")},
		{"parenthesized list_append", "a = (list_append(l, [1]))", oneOne},
		{"parenthesized list", "a = list_append((l), [1])", oneOne},
	}

	for i, row := range applied {
		id := "v" + strconv.Itoa(i)

		c.steps = append(c.steps, putSteps(table, chainItem(id))...)
		c.steps = append(c.steps,
			step{
				row.name, opStatement, stmt("UPDATE " + table + " SET " + row.set + " WHERE id = `" + id + "`"),
				http.StatusOK, noItems,
			},
			itemIs(table, id, withAttrs(chainItem(id), `"a":`+row.a)))
	}

	// The two actions of one UPDATE, each in a form of its own.
	c.steps = append(c.steps, putSteps(table, chainItem("two"))...)
	c.steps = append(c.steps,
		step{
			"two actions", opStatement,
			stmt("UPDATE " + table + " SET a = k0 + 1 + 1, b = list_append(list_append(l, [1]), [1]) WHERE id = `two`"),
			http.StatusOK, noItems,
		},
		itemIs(table, "two", withAttrs(chainItem("two"), `"a":`+num(7), `"b":`+listOfNumbers("1", "1", "1"))))

	refused := []struct{ name, set, msg string }{
		{"list_append in a sum", "a = list_append(l, [1]) + 1", notAList + "+, operand type: L"},
		{"list_append after a sum", "a = 1 + list_append(l, [1])", notAList + "+, operand type: L"},
		{"list in a sum", "a = (k0 + 1) + [1]", notAList + "+, operand type: L"},
		{"list after a list", "a = [1] + (k0 + 1)", notAList + "+, operand type: L"},
		{"lists in sums", "a = (1 + 1) + (1 + [1])", notAList + "+, operand type: L"},
		{"list_append in a difference", "a = list_append(l, [1]) - 1", notAList + "-, operand type: L"},
		{"list_append after a difference", "a = 1 - list_append(l, [1])", notAList + "-, operand type: L"},
		{"sum in list_append", "a = list_append(k0 + 1, [1])", notAList + numbersInList},
		{"sum last in list_append", "a = list_append(l, k0 + 1)", notAList + numbersInList},
		{"sum second in nested list_append", "a = list_append(list_append(l, [1]), 1 + 1)", notAList + numbersInList},
		{"number in list_append", "a = list_append(1, [1])", notAList + "list_append, operand type: N"},
		{
			"number in list_append in if_not_exists", "a = if_not_exists(zz, list_append(1, [1]))",
			notAList + "list_append, operand type: N",
		},
		{"sum for if_not_exists", "a = if_not_exists(k0 + 1, 1)", needsPath},
		{"string in a chain", "a = s + 1 + 1", wrongType},
		{"string last in a chain", "a = k0 + 1 + s", wrongType},
		{"string default in a sum", "a = if_not_exists(zz, 'x') + 1", wrongType},
		{"number default in list_append", "a = list_append(if_not_exists(zz, 5), [1])", wrongType},
		{"absent first in a chain", "a = zz + 1 + 1", missingRef},
		{"absent last in a chain", "a = k0 + 1 + zz", missingRef},
	}

	for i, row := range refused {
		id := "r" + strconv.Itoa(i)

		c.steps = append(c.steps, putSteps(table, chainItem(id))...)
		c.steps = append(c.steps,
			step{
				row.name, opStatement, stmt("UPDATE " + table + " SET " + row.set + " WHERE id = `" + id + "`"),
				http.StatusBadRequest, validation(row.msg),
			},
			itemIs(table, id, chainItem(id)))
	}

	return c
}

// Test_partiql_updateValueGrammar: the value of an UPDATE's SET action is an
// expression, applied as the image applies it: sums and differences chained,
// parenthesized or inside a function, and a list_append in a list_append or an
// if_not_exists. An UpdateExpression takes one + or - and list_append only as
// its whole value; a statement compiles to one, and refused these with a
// syntax error or "not allowed to be used this way". The refusals are the
// image's, a sum or a list_append of the wrong type named by its type.
func Test_partiql_updateValueGrammar(t *testing.T) {
	c := updateValueCase()

	s := newStack(t)
	s.ok(t, opCreate, limitTable(c.table))
	run(t, s, c.steps)
}

// chainKinds are the actions of an UPDATE that an UpdateExpression has no
// grammar for, with the operators each counts and the value each leaves in an
// attribute of operatorItem: k0 is 1 and l is [1].
var chainKinds = []struct {
	name      string
	action    func(i int) string
	operators int
	value     string
}{
	{"chained sums", func(i int) string { return "u" + strconv.Itoa(i) + "=k0+1+1" }, 2, number("3")},
	{"three sums", func(i int) string { return "u" + strconv.Itoa(i) + "=k0+1+1+1" }, 3, number("4")},
	{"difference of a sum", func(i int) string { return "u" + strconv.Itoa(i) + "=k0-(1+1)" }, 2, number("-1")},
	{
		"if_not_exists in a chain", func(i int) string { return "u" + strconv.Itoa(i) + "=if_not_exists(zz,1)+1+1" }, 3,
		number("3"),
	},
	{
		"nested list_append", func(i int) string { return "u" + strconv.Itoa(i) + "=list_append(list_append(l,[1]),[1])" },
		2, `{"L":[{"N":"1"},{"N":"1"},{"N":"1"}]}`,
	},
}

// chainCountCase is the rows of an UPDATE of the actions of chainKinds through
// each surface: 300 operators are applied and 301 or more refused with the item
// as it was, each + - and function call counted.
func chainCountCase() limitCase {
	const table = "pchains"

	c := limitCase{table: table}

	for _, kind := range chainKinds {
		for _, sf := range surfaces() {
			for _, n := range []int{300 / kind.operators, 300/kind.operators + 1} {
				id := strings.Join([]string{sf.name, strings.ReplaceAll(kind.name, " ", "-"), strconv.Itoa(n)}, "-")
				src := "UPDATE " + table + " SET " + repeat(n, ",", kind.action) + " WHERE id = '" + id + "'"
				status, want := sf.applied(table)
				after := updated(id, n, kind.value)

				if n*kind.operators > 300 {
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

// Test_partiql_updateChainsAreCounted: each + - and function call of an UPDATE's
// value is one of the 300 operators, whatever its depth: 150 actions of
// `k0 + 1 + 1` are applied and 151 refused, as the image answers, through
// ExecuteStatement, BatchExecuteStatement and ExecuteTransaction.
func Test_partiql_updateChainsAreCounted(t *testing.T) {
	c := chainCountCase()

	s := newStack(t)
	s.ok(t, opCreate, limitTable(c.table))
	run(t, s, c.steps)
}

// groupOf is a parenthesized condition of exactly operators operators, at least
// 4: comparisons of k0 to k7 joined by OR, with a NOT among them when the count
// is even.
func groupOf(operators int) string {
	term := func(i int) string { return "k" + strconv.Itoa(i%8) + " = 1" }

	if operators%2 == 1 {
		return "(" + repeat((operators+1)/2, " OR ", term) + ")"
	}

	return "(NOT (k0 = 1) OR " + repeat((operators-2)/2, " OR ", term) + ")"
}

// keyedTable is a table the WHERE rows run over: keyed by id, and by sk too when
// it has a range key.
type keyedTable struct {
	name   string
	ranged bool
}

// item is the item of key id: sk when the table has a range key, and k0 to k7 as
// 1.
func (k keyedTable) item(id string) string {
	if k.ranged {
		return withAttrs(whereItem(id), `"sk":{"S":"x"}`)
	}

	return whereItem(id)
}

// keys is a WHERE that names the item id by its whole key.
func (k keyedTable) keys(id string) string {
	if k.ranged {
		return "id = '" + id + "' AND sk = 'x'"
	}

	return "id = '" + id + "'"
}

// operators is what keys holds: an equality for each key attribute, and the AND
// that joins each to the group after it.
func (k keyedTable) operators() int {
	if k.ranged {
		return 4
	}

	return 2
}

// get is a GetItem step of the item id that expects item, the JSON of an item, or
// none.
func (k keyedTable) get(id, item string) step {
	if !k.ranged {
		return itemIs(k.name, id, item)
	}

	want := `{}`
	if item != "" {
		want = `{"Item":` + item + `}`
	}

	return step{
		"get " + id, opGet, `{"TableName":"` + k.name + `","Key":{"id":{"S":"` + id + `"},"sk":{"S":"x"}}}`,
		http.StatusOK, want,
	}
}

// create is the CreateTable body of the table.
func (k keyedTable) create() string {
	if k.ranged {
		return rangeKeyTable(k.name)
	}

	return limitTable(k.name)
}

// whereSelects are the statements of a SELECT asked as a Query with a WHERE of
// total operators: by the key alone, and on a table with a range key by a
// condition on it, a comparison and a BETWEEN, each one operator.
func (k keyedTable) whereSelects(total int) []step {
	n := strconv.Itoa(total)
	rows := []struct{ name, where string }{
		{"select " + n, k.keys("a") + " AND " + groupOf(total-k.operators())},
	}

	if k.ranged {
		rows = append(rows,
			struct{ name, where string }{
				"select above " + n, "id = 'a' AND sk > 'a' AND " + groupOf(total-4),
			},
			struct{ name, where string }{
				"select between " + n, "id = 'a' AND sk BETWEEN 'a' AND 'z' AND " + groupOf(total-4),
			})
	}

	steps := make([]step, len(rows))
	for i, row := range rows {
		steps[i] = step{
			row.name, opStatement, stmt("SELECT id FROM " + k.name + " WHERE " + row.where),
			countedStatus(total), countedBody(total, foundA, validation(tooManyOperators)),
		}
	}

	return steps
}

// foundA is a SELECT of id that found the item a.
const foundA = `{"Items":[{"id":{"S":"a"}}]}`

// whereCountCase is the rows of a WHERE that holds `total` operators, counted as
// written, the key equality and the AND after it included for each key
// attribute: a SELECT as a Query, an EXISTS, a DELETE and an UPDATE, which the
// image applies at 300 and refuses at 301.
func whereCountCase(k keyedTable) limitCase {
	c := limitCase{table: k.name, steps: putSteps(k.name, k.item("a"))}

	for _, total := range []int{300, 301} {
		n := strconv.Itoa(total)
		group := groupOf(total - k.operators())

		c.steps = append(c.steps, k.whereSelects(total)...)
		c.steps = append(c.steps, step{
			"exists " + n, opTransaction,
			`{"TransactStatements":[` + stmt("EXISTS(SELECT * FROM "+k.name+" WHERE "+k.keys("a")+" AND "+group+")") + `]}`,
			countedStatus(total), countedBody(total, noResponses,
				validation("Validation failed in TransactStatements[0]: "+tooManyOperators)),
		})

		for _, sf := range surfaces() {
			id := "w-" + sf.name + "-" + n
			dead := "d" + id
			update := "UPDATE " + k.name + " SET x = 1 WHERE " + k.keys(id) + " AND " + group
			remove := "DELETE FROM " + k.name + " WHERE " + k.keys(dead) + " AND " + group
			status, want := sf.applied(k.name)
			after, gone := withAttrs(k.item(id), `"x":{"N":"1"}`), ""

			if total > 300 {
				status, want = sf.refused(tooManyOperators)
				after, gone = k.item(id), k.item(dead)
			}

			c.steps = append(c.steps, putSteps(k.name, k.item(id))...)
			c.steps = append(c.steps, step{"update " + id, sf.op, sf.wrap(update), status, want}, k.get(id, after))
			c.steps = append(c.steps, putSteps(k.name, k.item(dead))...)
			c.steps = append(c.steps, step{"delete " + dead, sf.op, sf.wrap(remove), status, want}, k.get(dead, gone))
		}
	}

	return c
}

// countedStatus is the status of a call whose WHERE holds total operators.
func countedStatus(total int) int {
	if total > 300 {
		return http.StatusBadRequest
	}

	return http.StatusOK
}

// countedBody is applied for a WHERE of at most 300 operators and refused above.
func countedBody(total int, applied, refused string) string {
	if total > 300 {
		return refused
	}

	return applied
}

// Test_partiql_whereCountsEveryOperator: a WHERE is counted as written, the
// equality on each key attribute and the AND after it included, as the image
// counts it: `id = 'a' AND (` with 150 comparisons joined by OR `)` holds 301
// operators and is refused by a SELECT, an EXISTS, a DELETE and an UPDATE, on a
// table with a hash key and on one with a range key too. Counting the condition
// the statement compiles to, which holds no key equality, left a SELECT, an
// EXISTS and a DELETE 2 operators short for a hash key and 4 for a hash and a
// range key, and an UPDATE, whose condition holds an attribute_exists and an AND
// in their place, 2 short for a range key.
func Test_partiql_whereCountsEveryOperator(t *testing.T) {
	for _, k := range []keyedTable{{"pcounth", false}, {"pcountr", true}} {
		t.Run(k.name, func(t *testing.T) {
			c := whereCountCase(k)

			s := newStack(t)
			s.ok(t, opCreate, k.create())
			run(t, s, c.steps)
		})
	}
}

// Test_partiql_whereParenthesesAreNotCapped: a WHERE of 4,000 parentheses nested,
// an 8,055-byte statement, is answered as one of 3,000 is. The image answers it
// `500 Internal Server Error`, a crash that docs/compat.md names and gostack does
// not copy (SQ-454 ruling).
func Test_partiql_whereParenthesesAreNotCapped(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, limitTable("pdeep"))

	steps := putSteps("pdeep", `{"id":{"S":"a"}}`)

	for _, depth := range []int{3000, 4000} {
		src := "SELECT id FROM pdeep WHERE " + strings.Repeat("(", depth) + "id = 'a'" + strings.Repeat(")", depth)

		steps = append(steps, step{
			strconv.Itoa(depth) + " deep", opStatement, stmt(src), http.StatusOK, foundA,
		})
	}

	run(t, s, steps)
}

// whereSelectCase is the rows of a SELECT by its whole key with more in its WHERE
// through a batch and a transaction, which refuse it for what it is, a read that
// is not one item's, after the operators of the WHERE are counted: at 301 the
// count is the refusal, as the image answers.
func whereSelectCase() limitCase {
	const table = "pwsel"

	c := limitCase{table: table, steps: putSteps(table, whereItem("a"))}

	for _, total := range []int{300, 301} {
		src := "SELECT id FROM " + table + " WHERE id = 'a' AND " + groupOf(total-2)
		batchBody, batchWant := `{"Statements":[`+stmt(src)+`]}`,
			`{"Responses":[{"Error":{"Code":"ValidationError","Message":"Only single item select is supported"}}]}`
		txBody := `{"TransactStatements":[` + stmt(src) + `]}`
		txStatus, txWant := http.StatusBadRequest,
			validation("Validation failed in TransactStatements[0]: Select statements within ExecuteTransaction "+
				"must specify the primary key in the where clause.")

		if total > 300 {
			batchWant = `{"Responses":[{"Error":{"Code":"ValidationError","Message":` + jsonString(tooManyOperators) + `}}]}`
			txWant = validation("Validation failed in TransactStatements[0]: " + tooManyOperators)
		}

		n := strconv.Itoa(total)
		c.steps = append(c.steps,
			step{"batch select " + n, opBatchStmt, batchBody, http.StatusOK, batchWant},
			step{"transaction select " + n, opTransaction, txBody, txStatus, txWant})
	}

	return c
}

// Test_partiql_whereCountsASelectOfOneItem: a batch and a transaction count the
// operators of a SELECT of one item's key with a filter before they refuse it.
func Test_partiql_whereCountsASelectOfOneItem(t *testing.T) {
	c := whereSelectCase()

	s := newStack(t)
	s.ok(t, opCreate, limitTable(c.table))
	run(t, s, c.steps)
}
