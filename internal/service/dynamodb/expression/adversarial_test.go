package expression_test

import (
	"strings"
	"testing"
	"time"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/kylecannon/gostack/internal/service/dynamodb/table"
	"github.com/stretchr/testify/require"
)

// adversarial are the deepest nestings each parser can be handed: at the
// 4,096-character limit, and far past it as a request body could carry.
func adversarial() map[string]string {
	const huge = 1 << 20

	return map[string]string{
		"parens at limit":  strings.Repeat("(", 2040) + "a = :v" + strings.Repeat(")", 2040),
		"not at limit":     strings.Repeat("NOT ", 1020) + "a = :v",
		"size at limit":    strings.Repeat("size(", 680) + "a" + strings.Repeat(")", 680) + " = :v",
		"parens huge":      strings.Repeat("(", huge) + "a = :v" + strings.Repeat(")", huge),
		"if_not_exists at": "SET a = " + strings.Repeat("if_not_exists(a, ", 240) + ":v" + strings.Repeat(")", 240),
		"if_not_exists huge": "SET a = " + strings.Repeat("if_not_exists(a, ", huge/16) + ":v" +
			strings.Repeat(")", huge/16),
		"update parens huge": "SET a = " + strings.Repeat("(", huge) + ":v" + strings.Repeat(")", huge),
	}
}

// Test_adversarial_boundedNesting: every parser answers the deepest input it
// can be given, at the size limit and far beyond it, with an error or a
// result, quickly and without running out of stack.
func Test_adversarial_boundedNesting(t *testing.T) {
	subs := expression.Substitutions{Values: map[string]api.AttributeValue{":v": {S: new("x")}}}
	key := table.KeySchema{Hash: table.KeyAttribute{Name: "a", Type: "S"}}

	parsers := map[string]func(string) error{
		"condition": func(src string) error {
			_, err := expression.ParseCondition(expression.ConditionExpression, src, subs, map[string]struct{}{})

			return err //nolint:wrapcheck // the parser's error, as the test reads it
		},
		"key condition": func(src string) error {
			_, err := expression.ParseKeyCondition(src, key, subs, map[string]struct{}{})

			return err //nolint:wrapcheck // the parser's error, as the test reads it
		},
		"projection": func(src string) error {
			_, err := expression.ParseProjection(src, subs, map[string]struct{}{})

			return err //nolint:wrapcheck // the parser's error, as the test reads it
		},
		"update": func(src string) error {
			_, err := expression.ParseUpdate(src, subs, map[string]struct{}{})

			return err //nolint:wrapcheck // the parser's error, as the test reads it
		},
	}

	for parser, parse := range parsers {
		for name, src := range adversarial() {
			t.Run(parser+"/"+name, func(t *testing.T) {
				start := time.Now()
				_ = parse(src)
				require.Less(t, time.Since(start), 2*time.Second)
			})
		}
	}

	_, err := expression.ParseUpdate(adversarial()["update parens huge"], subs, map[string]struct{}{})
	require.EqualError(t, err, "ValidationException: Invalid UpdateExpression: "+
		"Expression size has exceeded the maximum allowed size;")
}
