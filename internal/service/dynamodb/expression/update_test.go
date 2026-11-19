package expression_test

import (
	"encoding/json"
	"testing"

	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

const (
	// updateKey is the key of every item the update tests write.
	updateKey = `{"pk":{"S":"k"}}`
	// updateX is the values of most error rows: :v, the string x.
	updateX = `{":v":{"S":"x"}}`
	// updateN is :n, the number 1.
	updateN = `{":n":{"N":"1"}}`
	// updateAddS is an ADD of :v to s.
	updateAddS = "ADD s :v"
	// updateXY is the values x and y.
	updateXY = `{":v":{"S":"x"},":w":{"S":"y"}}`
	// updateList012 is the list 0, 1, 2 in a.
	updateList012 = `{"a":{"L":[{"S":"0"},{"S":"1"},{"S":"2"}]}}`
	// updateAddN is an ADD of :v to n.
	updateAddN = "ADD n :v"
	// updateV1 is :v, the number 1.
	updateV1 = `{":v":{"N":"1"}}`
	// updateDeleteS is a DELETE of :v from s.
	updateDeleteS = "DELETE s :v"
	// updateSA is the string set a in s.
	updateSA = `{"s":{"SS":["a"]}}`
	// updateAX is the string x in a.
	updateAX = `{"a":{"S":"x"}}`
	// updateAQ is the string q in a.
	updateAQ = `{"a":{"S":"q"}}`
)

// updateItem decodes a JSON item and validates it, as the provider does before
// an item reaches the engine. An empty string is no item.
func updateItem(t *testing.T, s string) value.Item {
	t.Helper()

	if s == "" {
		return nil
	}

	var item value.Item
	require.NoError(t, json.Unmarshal([]byte(s), &item))
	require.NoError(t, value.ValidateItem(item))

	return item
}

// updateSubs builds the substitutions of a test from JSON names and values.
func updateSubs(t *testing.T, names, values string) expression.Substitutions {
	t.Helper()

	subs := expression.Substitutions{Values: updateItem(t, values)}
	if names != "" {
		require.NoError(t, json.Unmarshal([]byte(names), &subs.Names))
	}

	return subs
}

// updateJSON encodes an item to compare it, or "" for none.
func updateJSON(t *testing.T, item value.Item) string {
	t.Helper()

	if item == nil {
		return ""
	}

	b, err := json.Marshal(item)
	require.NoError(t, err)

	return string(b)
}

// requireUpdateError checks err is DynamoDB's ValidationException with msg.
func requireUpdateError(t *testing.T, err error, msg string) {
	t.Helper()

	var awsErr *awserr.Error
	require.ErrorAs(t, err, &awsErr)
	require.Equal(t, "ValidationException", awsErr.Code)
	require.Equal(t, msg, awsErr.Message)
}

// Every message below was observed from DynamoDB Local in the LocalStack image
// (gostack-parity-localstack:8b9a79f) for the same expression; rows that hold
// two problems pin which one DynamoDB reports first.
func Test_ParseUpdate_Errors(t *testing.T) {
	t.Parallel()

	const (
		bad      = "Invalid UpdateExpression: "
		syntax   = bad + "Syntax error; "
		rewrite  = "must remove or rewrite one of these paths; "
		overlap  = bad + "Two document paths overlap with each other; " + rewrite
		conflict = bad + "Two document paths conflict with each other; " + rewrite
		operand  = bad + "Incorrect operand type for operator or function; "
		arity    = bad + "Incorrect number of operands for operator or function; operator or function: "
		reserved = bad + "Attribute name is a reserved keyword; reserved keyword: "
		noName   = bad + "An expression attribute name used in the document path is not defined; attribute name: "
		noValue  = bad + "An expression attribute value used in expression is not defined; attribute value: "
		twoSets  = bad + `The "SET" section can only be used once in an update expression;`
		deep     = "a.b.c.d.e.f.g.h.i.j.k.l.m.n.o.p.q.r.s.t.u.v.w.x.y.z.a1.b1.c1.d1.e1.f1.g1"
		addType  = operand + "operator: ADD, operand type: "
		delType  = operand + "operator: DELETE, operand type: "
		emptyL   = `{":v":{"L":[]}}`
	)

	tests := []struct {
		name       string
		giveExpr   string
		giveNames  string
		giveValues string
		wantMsg    string
	}{
		{"empty", "", "", "", bad + "The expression can not be empty;"},
		{"only spaces", "   ", "", "", syntax + `token: "<EOF>", near: "   "`},
		{"equals first", "SET = :v", "", updateX, syntax + `token: "=", near: "SET = :v"`},
		{"trailing comma", "SET a = :v,", "", updateX, syntax + `token: "<EOF>", near: ","`},
		{"missing equals", "SET a :v", "", updateX, syntax + `token: ":v", near: "a :v"`},
		{"spacing kept", "SET   a   =    =  :v", "", updateX, syntax + `token: "=", near: "=    =  :v"`},
		{"unknown clause", "PUT a = :v", "", updateX, syntax + `token: "PUT", near: "PUT a"`},
		{"clause alone", "SET", "", "", syntax + `token: "<EOF>", near: "SET"`},
		{"missing comma", "SET a = :v, b = :v c = :v", "", updateX, syntax + `token: "c", near: ":v c ="`},
		{"stray character", "SET a$b = :v", "", updateX, syntax + `token: "$", near: "a$b"`},
		{"underscore start", "SET a_1 = :v, _b = :v", "", updateX, syntax + `token: "_", near: ", _b"`},
		{"digit start", "SET 1a = :v", "", updateX, syntax + `token: "1", near: "SET 1a"`},
		{"leading zero index", "SET a[01] = :v", "", updateX, syntax + `token: "1", near: "01]"`},
		{"negative index", "SET a[-1] = :v", "", updateX, syntax + `token: "-", near: "[-1"`},
		{"value as path", "SET :v = :v", "", updateX, syntax + `token: ":v", near: "SET :v ="`},
		{"three terms", "SET a = :n + :n + :n", "", updateN, syntax + `token: "+", near: ":n + :n"`},
		{"sum as argument", "SET a = list_append(:n + :n, :n)", "", updateN, syntax + `token: "+", near: ":n + :n"`},
		{"path as ADD value", "ADD s b", "", "", syntax + `token: "b", near: "s b"`},
		{"empty clause", "SET a = :v REMOVE", "", updateX, syntax + `token: "<EOF>", near: "REMOVE"`},
		{"syntax before undefined", "SET a = :nope,", "", updateX, syntax + `token: "<EOF>", near: ","`},
		{
			"index too large", "SET a[2147483648] = :v, name = :v", "", updateX,
			bad + "List index is not within the allowable range; index: [2147483648]",
		},
		{"reserved word", "SET a = :nope SET name = :v", "", updateX, reserved + "name"},
		{"reserved word as written", "REMOVE a.Status", "", "", reserved + "Status"},
		{"reserved before function", "SET name = foo(:v)", "", updateX, reserved + "name"},
		{"unknown function", "SET a = foo(:v) SET b = :v", "", updateX, bad + "Invalid function name; function: foo"},
		{
			"function name case", "SET a = IF_NOT_EXISTS(a, :v)", "", updateX,
			bad + "Invalid function name; function: IF_NOT_EXISTS",
		},
		{
			"condition function", "SET a = size(b)", "", "",
			bad + "The function is not allowed in an update expression; function: size",
		},
		{"repeated clause", "set a = :v remove b set c = :v", "", updateX, twoSets},
		{"repeated before depth", "SET " + deep + " = :v SET b = :v", "", updateX, twoSets},
		{
			"too deep", "SET a" + "[0][0][0][0][0][0][0][0][0][0][0][0][0][0][0][0]" +
				"[0][0][0][0][0][0][0][0][0][0][0][0][0][0][0][0] = :v", "", updateX,
			bad + "The document path has too many nesting levels; nesting levels: 33",
		},
		{"undefined name", "SET a = :nope, #nope = :v", "", updateX, noName + "#nope"},
		{"undefined name before arity", "SET a = list_append(#nope)", "", "", noName + "#nope"},
		{"overlap", "SET a = :v, a.b = :nope", "", updateX, overlap + "path one: [a], path two: [a, b]"},
		{"overlap same path", "SET a = :v REMOVE a", "", updateX, overlap + "path one: [a], path two: [a]"},
		{
			"overlap through names", "SET #a = :v, #b.c = :v", `{"#a":"x","#b":"x"}`, updateX,
			overlap + "path one: [x], path two: [x, c]",
		},
		{
			"overlap at index", "SET a[1] = :v, a[1].b = :v", "", updateX,
			overlap + "path one: [a, [1]], path two: [a, [1], b]",
		},
		{"conflict", "SET a.b = :v, a[1] = :v", "", updateX, conflict + "path one: [a, b], path two: [a, [1]]"},
		{
			"overlap before operand type", "SET a = list_append(:v, :v), a.b = :v", "", updateX,
			overlap + "path one: [a], path two: [a, b]",
		},
		{"undefined value", "SET a = :nope", "", updateX, noValue + ":nope"},
		{
			"path required", "SET a = if_not_exists(:v, :v)", "", updateX,
			bad + "Operator or function requires a document path; operator or function: if_not_exists",
		},
		{"one operand", "SET a = if_not_exists(b)", "", "", arity + "if_not_exists, number of operands: 1"},
		{"three operands", "SET a = list_append(:v, :v, :v)", "", emptyL, arity + "list_append, number of operands: 3"},
		{
			"list_append string", "SET a = list_append(:w, :v)", "", `{":v":{"S":"x"},":w":{"L":[]}}`,
			operand + "operator or function: list_append, operand type: S",
		},
		{
			"list_append string set", "SET a = list_append(:v, :v)", "", `{":v":{"SS":["1"]}}`,
			operand + "operator or function: list_append, operand type: SS",
		},
		{
			"plus string", "SET a = :v + :n", "", `{":v":{"S":"x"},":n":{"N":"1"}}`,
			operand + "operator or function: +, operand type: S",
		},
		{
			"minus list", "SET a = :n - :v", "", `{":v":{"L":[]},":n":{"N":"1"}}`,
			operand + "operator or function: -, operand type: L",
		},
		{"ADD string", updateAddS, "", `{":v":{"S":"a"}}`, addType + "STRING, typeSet: ALLOWED_FOR_ADD_OPERAND"},
		{"ADD boolean", updateAddS, "", `{":v":{"BOOL":true}}`, addType + "BOOLEAN, typeSet: ALLOWED_FOR_ADD_OPERAND"},
		{"ADD map", updateAddS, "", `{":v":{"M":{}}}`, addType + "MAP, typeSet: ALLOWED_FOR_ADD_OPERAND"},
		{"DELETE number", updateDeleteS, "", updateV1, delType + "NUMBER, typeSet: ALLOWED_FOR_DELETE_OPERAND"},
		{"DELETE list", updateDeleteS, "", emptyL, delType + "LIST, typeSet: ALLOWED_FOR_DELETE_OPERAND"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := expression.ParseUpdate(tt.giveExpr, updateSubs(t, tt.giveNames, tt.giveValues), nil)
			requireUpdateError(t, err, tt.wantMsg)
		})
	}
}

func Test_ParseUpdate_Used(t *testing.T) {
	t.Parallel()

	subs := updateSubs(t, `{"#a":"a","#b":"b","#c":"c"}`, `{":v":{"S":"x"},":w":{"N":"1"},":z":{"S":"z"}}`)
	used := map[string]struct{}{}

	u, err := expression.ParseUpdate("SET #a = if_not_exists(#b, :v) ADD n :w", subs, used)
	require.NoError(t, err)
	require.Equal(t, map[string]struct{}{"#a": {}, "#b": {}, ":v": {}, ":w": {}}, used)
	require.Equal(t, []expression.Path{{{Name: "a"}}, {{Name: "n"}}}, u.Paths())

	requireUpdateError(t, expression.CheckUnused(subs, used),
		"Value provided in ExpressionAttributeNames unused in expressions: keys: {#c}")

	used["#c"] = struct{}{}
	requireUpdateError(t, expression.CheckUnused(subs, used),
		"Value provided in ExpressionAttributeValues unused in expressions: keys: {:z}")

	used[":z"] = struct{}{}
	require.NoError(t, expression.CheckUnused(subs, used))
}

// DynamoDB Local lists unused keys in its hash set's order: {:z, :w}.
func Test_CheckUnused_Order(t *testing.T) {
	t.Parallel()

	subs := updateSubs(t, "", `{":v":{"S":"x"},":w":{"S":"y"},":z":{"S":"y"}}`)
	err := expression.CheckUnused(subs, map[string]struct{}{":v": {}})
	requireUpdateError(t, err, "Value provided in ExpressionAttributeValues unused in expressions: keys: {:z, :w}")
}
