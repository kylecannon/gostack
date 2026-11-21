package expression_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/stretchr/testify/require"
)

// tooManyOperators is the message DynamoDB Local answers an UpdateExpression of
// more than 300 operators with, whatever the count: it stops at 301 (SQ-452,
// the image gostack-parity-localstack:8b9a79f, direct UpdateItem).
const tooManyOperators = "Invalid UpdateExpression: The expression contains too many operators; operator count: 301"

// The right-hand sides the rows below repeat, one action each.
const (
	sumTerm    = "k0+:n"
	diffTerm   = "k0-:n"
	appendTerm = "list_append(l,:l)"
	absentTerm = "if_not_exists(k0,:n)"
	plainTerm  = ":n"
)

// operatorSubs is :n, the number 1, :s, a string, and :l, a list.
const operatorSubs = `{":n":{"N":"1"},":s":{"S":"x"},":l":{"L":[{"N":"1"}]}}`

// actions is "SET " and n actions, u<i> = term.
func actions(n int, term string) string {
	return "SET " + indexed(n, func(i string) string { return "u" + i + "=" + term })
}

// indexed is n terms, term("0") to term("<n-1>"), joined by commas.
func indexed(n int, term func(i string) string) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = term(strconv.Itoa(i))
	}

	return strings.Join(parts, ",")
}

// Test_ParseUpdate_OperatorCount: an UpdateExpression of 301 operators is
// refused and one of 300 parses, as the image does. Each + and -, and each
// call of list_append and if_not_exists, is one operator; a plain SET, ADD,
// REMOVE and the elements of a path are none. A PartiQL UPDATE, which no size
// holds, is counted as a request's UpdateExpression is.
func Test_ParseUpdate_OperatorCount(t *testing.T) {
	t.Parallel()

	// sums are the first actions and list_append calls the rest.
	mixed := func(sums, total int) string {
		return actions(sums, sumTerm) + "," + indexed(total-sums, func(i string) string {
			return "v" + i + "=" + appendTerm
		})
	}

	tests := []struct {
		name     string
		giveExpr string
		wantErr  bool
	}{
		{"300 sums", actions(300, sumTerm), false},
		{"301 sums", actions(301, sumTerm), true},
		{"1,000 sums", actions(1000, sumTerm), true},
		{"300 differences", actions(300, diffTerm), false},
		{"301 differences", actions(301, diffTerm), true},
		{"300 list_append", actions(300, appendTerm), false},
		{"301 list_append", actions(301, appendTerm), true},
		{"300 if_not_exists", actions(300, absentTerm), false},
		{"301 if_not_exists", actions(301, absentTerm), true},
		{"200 sums and 100 list_append", mixed(200, 300), false},
		{"200 sums and 101 list_append", mixed(200, 301), true},
		{"150 if_not_exists with a sum each", actions(150, absentTerm+"+:n"), false},
		{"151 if_not_exists with a sum each", actions(151, absentTerm+"+:n"), true},
		{"301 parenthesized sums", actions(301, "("+sumTerm+")"), true},
		{"300 sums and a REMOVE", actions(300, sumTerm) + " REMOVE r", false},
		{"301 plain SET", actions(301, plainTerm), false},
		{"301 nested SET", "SET " + indexed(301, func(i string) string { return "m.u" + i + "=:n" }), false},
		{"301 ADD", "ADD " + indexed(301, func(i string) string { return "c" + i + " :n" }), false},
		{"301 REMOVE", "REMOVE " + indexed(301, func(i string) string { return "r" + i }), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			subs := updateSubs(t, "", operatorSubs)
			want := func(t *testing.T, err error) {
				t.Helper()

				if tt.wantErr {
					requireUpdateError(t, err, tooManyOperators)

					return
				}

				require.NoError(t, err)
			}

			// The size of a statement's expression is not held.
			_, err := expression.ParseStatementUpdate(tt.giveExpr, subs, nil, nil)
			want(t, err)

			// A request's is, so the expressions that fit it are parsed both ways.
			if len(tt.giveExpr) <= 4096 {
				_, err = expression.ParseUpdate(tt.giveExpr, subs, nil)
				want(t, err)
			}
		})
	}
}

// Test_ParseUpdate_OperatorCountLast: DynamoDB Local counts the operators after
// every other check, so 301 of them with a second mistake answer the mistake. The
// rows are the image's answers for 301 sums and one more action.
//
//nolint:lll // each row holds the exact text the LocalStack image answered
func Test_ParseUpdate_OperatorCountLast(t *testing.T) {
	t.Parallel()

	const bad = "Invalid UpdateExpression: "

	sums := actions(301, sumTerm)

	tests := []struct {
		name     string
		giveExpr string
		wantMsg  string
	}{
		{"syntax", sums + ",=:n", bad + `Syntax error; token: "=", near: ",=:n"`},
		{"reserved word", sums + ",name=:n", bad + "Attribute name is a reserved keyword; reserved keyword: name"},
		{"function", sums + ",x=foo(k0)", bad + "Invalid function name; function: foo"},
		{"repeated clause", sums + " SET z=:n", bad + `The "SET" section can only be used once in an update expression;`},
		{"depth", sums + "," + strings.Repeat("a.", 32) + "a=:n", bad + "The document path has too many nesting levels; nesting levels: 33"},
		{"overlap", sums + ",u0.x=:n", bad + "Two document paths overlap with each other; must remove or rewrite one of these paths; path one: [u0], path two: [u300]"},
		{"operand type", sums + ",w=:s+:n", bad + "Incorrect operand type for operator or function; operator or function: +, operand type: S"},
		{"ADD type", sums + " ADD c :s", bad + "Incorrect operand type for operator or function; operator: ADD, operand type: STRING, typeSet: ALLOWED_FOR_ADD_OPERAND"},
		{"function arity", actions(300, sumTerm) + ",v=if_not_exists(k0)", bad + "Incorrect number of operands for operator or function; operator or function: if_not_exists, number of operands: 1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := expression.ParseStatementUpdate(tt.giveExpr, updateSubs(t, "", operatorSubs), nil, nil)
			requireUpdateError(t, err, tt.wantMsg)
		})
	}
}
