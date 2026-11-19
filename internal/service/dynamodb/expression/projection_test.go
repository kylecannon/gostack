package expression_test

import (
	"strings"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

//nolint:ireturn,wrapcheck // returns what the parser under test returns
func parseProjection(src string, subs *expression.Substitutions) (expression.Projection, error) {
	s := autoSubs(src)
	if subs != nil {
		s = *subs
	}

	used := map[string]struct{}{}

	pr, err := expression.ParseProjection(src, s, used)
	if err != nil {
		return nil, err
	}

	return pr, expression.CheckUnused(s, used)
}

// TestParseProjectionErrors holds the projection checks to the code and
// message the LocalStack image answered for each GetItem.
//
//nolint:lll,goconst // each row holds the exact text the LocalStack image answered; naming or splitting it hides what was probed
func TestParseProjectionErrors(t *testing.T) {
	t.Parallel()

	const prefix = "Invalid ProjectionExpression: "

	tests := []struct {
		name string
		src  string
		subs *expression.Substitutions
		want string
	}{
		{"ok", "a, pk, b.c[0]", nil, ""},
		{"duplicate", "a, a", nil, prefix + "Two document paths overlap with each other; must remove or rewrite one of these paths; path one: [a], path two: [a]"},
		{"duplicate nested", "a.b, a.b", nil, prefix + "Two document paths overlap with each other; must remove or rewrite one of these paths; path one: [a, b], path two: [a, b]"},
		{"parent after", "a.b, a", nil, prefix + "Two document paths overlap with each other; must remove or rewrite one of these paths; path one: [a, b], path two: [a]"},
		{"child after", "a[0], a[0].b", nil, prefix + "Two document paths overlap with each other; must remove or rewrite one of these paths; path one: [a, [0]], path two: [a, [0], b]"},
		{"first overlap", "x, a.b.c, y, a.b", nil, prefix + "Two document paths overlap with each other; must remove or rewrite one of these paths; path one: [a, b, c], path two: [a, b]"},
		{"substituted overlap", "#a, a", &expression.Substitutions{Names: map[string]string{"#a": "a"}}, prefix + "Two document paths overlap with each other; must remove or rewrite one of these paths; path one: [a], path two: [a]"},
		{"siblings", "a[0], a[1]", nil, ""},
		{"map siblings", "a.b, a.c", nil, ""},
		{"index siblings", "a[0][1], a[0][2]", nil, ""},
		{"names", "#a.#b", &expression.Substitutions{Names: map[string]string{"#a": "a", "#b": "b"}}, ""},
		{"reserved", "name", nil, prefix + "Attribute name is a reserved keyword; reserved keyword: name"},
		{"undefined", "#n", &expression.Substitutions{Names: map[string]string{"#m": "x"}}, prefix + "An expression attribute name used in the document path is not defined; attribute name: #n"},
		{"empty text", "", nil, prefix + "The expression can not be empty;"},
		{"whitespace", strings.Repeat(" ", 3), nil, prefix + `Syntax error; token: "<EOF>", near: "` + strings.Repeat(" ", 3) + `"`},
		{"trailing comma", "a,", nil, prefix + `Syntax error; token: "<EOF>", near: ","`},
		{"value", ":v", nil, prefix + `Syntax error; token: ":v", near: ":v"`},
		{"function", "size(a)", nil, prefix + `Syntax error; token: "(", near: "size(a"`},
		{"keyword", "a.AND", nil, prefix + `Syntax error; token: "AND", near: ".AND"`},
		{"depth", join(33, "a", "."), nil, prefix + "The document path has too many nesting levels; nesting levels: 33"},
		{"size 4096", strings.Repeat("a", 4096), nil, ""},
		{"size 4097", strings.Repeat("a", 4097), nil, prefix + "Expression size has exceeded the maximum allowed size;"},
		{"size before syntax", "a,," + strings.Repeat("a", 4096), nil, prefix + "Expression size has exceeded the maximum allowed size;"},
		{"unused name", "#a", &expression.Substitutions{Names: map[string]string{"#a": "a", "#b": "b"}}, "Value provided in ExpressionAttributeNames unused in expressions: keys: {#b}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseProjection(tt.src, tt.subs)
			wantMessage(t, err, tt.want)
		})
	}
}

// TestProjectionApply holds Apply to the items the image returned for the
// same projections of the same item.
func TestProjectionApply(t *testing.T) {
	t.Parallel()

	item := value.Item{
		"pk": str("x"), "sk": num("1"), "a": str("abc"), "n": num("5"),
		"l": {L: []api.AttributeValue{str("a"), num("1"), {M: map[string]api.AttributeValue{"b": str("z"), "c": str("y")}}}},
		"m": {M: map[string]api.AttributeValue{"b": str("z"), "c": {L: []api.AttributeValue{num("1"), num("2")}}}},
	}

	m := func(kv map[string]api.AttributeValue) api.AttributeValue { return api.AttributeValue{M: kv} }
	l := func(vs ...api.AttributeValue) api.AttributeValue { return api.AttributeValue{L: vs} }

	tests := []struct {
		src  string
		want value.Item
	}{
		{"l[2].c, l[0]", value.Item{"l": l(str("a"), m(map[string]api.AttributeValue{"c": str("y")}))}},
		{"m.zz, a", value.Item{"a": str("abc")}},
		{"l[0].b, m.c[1], m.c[5]", value.Item{"m": m(map[string]api.AttributeValue{"c": l(num("2"))})}},
		{"zz", value.Item{}},
		{"a[0], n.b", value.Item{}},
		{"m.b, m.c[0], pk", value.Item{
			"pk": str("x"), "m": m(map[string]api.AttributeValue{"b": str("z"), "c": l(num("1"))}),
		}},
		{"l", value.Item{"l": item["l"]}},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			t.Parallel()

			pr, err := parseProjection(tt.src, nil)
			if err != nil {
				t.Fatal(err)
			}

			got := pr.Apply(item)
			if !value.EqualItems(got, tt.want) {
				t.Fatalf("Apply = %v, want %v", got, tt.want)
			}

			if len(pr.Paths()) != strings.Count(tt.src, ",")+1 {
				t.Fatalf("Paths() = %v", pr.Paths())
			}
		})
	}
}
