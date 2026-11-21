package expression_test

import (
	"strconv"
	"testing"

	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/stretchr/testify/require"
)

// Test_ParseStatementCondition: the WHERE of a PartiQL statement is held to no
// size, so a filter of 12 IN lists of 100 values, 5,903 characters, parses and
// reads the item, where a request's FilterExpression of it is refused for its
// size first. The operator count and the operand limits hold as for any
// condition (SQ-452; the image answers the statement 200).
func Test_ParseStatementCondition(t *testing.T) {
	t.Parallel()

	list := func(last string) string { return "n IN (" + join(99, ":n1", ",") + "," + last + ")" }
	src := join(11, list(":n1"), " OR ") + " OR " + list(":n5x")
	subs := autoSubs(src)

	require.Greater(t, len(src), 4096)

	_, err := expression.ParseCondition(expression.FilterExpression, src, subs, nil)
	wantMessage(t, err, "Invalid FilterExpression: Expression size has exceeded the maximum allowed size; "+
		"expression size: "+strconv.Itoa(len(src)))

	for _, kind := range []expression.Kind{expression.FilterExpression, expression.ConditionExpression} {
		c, err := expression.ParseStatementCondition(kind, src, subs, nil)
		wantMessage(t, err, "")

		held, err := c.Eval(richItem())
		require.NoError(t, err)
		require.True(t, held, kind)
	}
}

// Test_ParseStatementCondition_Limits: what the size lift leaves in force.
func Test_ParseStatementCondition_Limits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		giveExpr string
		wantMsg  string
	}{
		{
			"operators", join(160, "n = :n1", " OR "),
			"Invalid ConditionExpression: The expression contains too many operators; operator count: 301",
		},
		{
			"IN operands", "n IN (" + join(101, ":n1", ",") + ")",
			"Invalid ConditionExpression: The IN operator is provided with too many operands; number of operands: 101",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := expression.ParseStatementCondition(
				expression.ConditionExpression, tt.giveExpr, autoSubs(tt.giveExpr), nil)
			wantMessage(t, err, tt.wantMsg)
		})
	}
}
