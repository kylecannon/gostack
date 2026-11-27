package dynamodb

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/partiql"
	"github.com/stretchr/testify/require"
)

// The #names and :values in rendered expression text.
var (
	namePlaceholder  = regexp.MustCompile(`#n[0-9]+`)
	valuePlaceholder = regexp.MustCompile(`:v[0-9]+`)
)

// Test_render_unknownKind: a node of a kind no rendering knows is an error from
// operand and from cond alike, which end there and do not hand it to each
// other.
func Test_render_unknownKind(t *testing.T) {
	unknown := &partiql.Node{Kind: partiql.Kind(-1)}

	_, err := newRender(nil).operand(unknown)
	require.ErrorIs(t, err, errNodeKind)

	_, err = newRender(nil).cond(unknown)
	require.ErrorIs(t, err, errNodeKind)

	// Under a condition too, where cond hands it on as an operand.
	_, err = newRender(nil).cond(&partiql.Node{Kind: partiql.KindNot, Args: []*partiql.Node{unknown}})
	require.ErrorIs(t, err, errNodeKind)
}

// isLogic reports whether k is a kind of condition.
func isLogic(k partiql.Kind) bool {
	switch k {
	case partiql.KindAnd, partiql.KindOr, partiql.KindNot, partiql.KindCompare, partiql.KindBetween,
		partiql.KindIn, partiql.KindIsNull, partiql.KindIsMissing:
		return true
	default:
		return false
	}
}

// callArity is how many arguments each function the renderer passes on takes; a
// call of any other count has no reading as an expression.
var callArity = map[string]int{
	"begins_with": 2, "contains": 2, "attribute_type": 2, "size": 1, "attribute_exists": 1,
	"attribute_not_exists": 1, "list_append": 2, "if_not_exists": 2, "set_add": 2, "set_delete": 2,
}

// regularStatement reports whether stmt's WHERE and the values of its actions
// are regular.
func regularStatement(stmt *partiql.Statement) bool {
	if stmt.Where != nil && (nonCondition(stmt.Where) || !regular(stmt.Where)) {
		return false
	}

	for _, a := range stmt.Actions {
		if a.Value != nil && !regular(a.Value) {
			return false
		}
	}

	return true
}

// regular reports whether n renders to text the expression parser reads:
// conditions joined by AND, OR and NOT, over comparisons of operands, and
// functions of the number of arguments they take. The parser of statements
// reads more than that (a bare operand under an AND, a call of no arguments),
// and the text it is rendered to then has no reading.
func regular(n *partiql.Node) bool {
	if n.Kind == partiql.KindCall {
		arity, known := callArity[n.Name]
		if !known || len(n.Args) != arity {
			return false
		}
	}

	switch n.Kind {
	case partiql.KindAnd, partiql.KindOr, partiql.KindNot:
		for _, a := range n.Args {
			boolCall := a.Kind == partiql.KindCall && a.Name != "size"
			if (!isLogic(a.Kind) && !boolCall) || !regular(a) {
				return false
			}
		}
	default:
		for _, a := range n.Args {
			if isLogic(a.Kind) || !regular(a) {
				return false
			}
		}
	}

	return true
}

// FuzzPartiQLRender: a statement the parser accepts is rendered to the end,
// its WHERE as a condition, its actions as an update and its list as a
// projection, with an answer or an error and never a panic or a render that
// does not end. What it renders is checked: every #name and :value its texts
// use is one the render handed back and every one it handed back is used, and
// the condition and the update parse as expressions, but where the statement
// writes a bare operand under an AND (regular). The seed corpus is the f.Add
// statements below.
func FuzzPartiQLRender(f *testing.F) {
	for _, src := range []string{
		"SELECT * FROM t WHERE id = 'a'",
		"SELECT a, m['x'], l[0] FROM t WHERE id = 'a' AND (x > 1 OR NOT y = 2)",
		"SELECT * FROM t WHERE id = 'a' AND x = y + 1",
		"SELECT * FROM t WHERE id = ? AND begins_with(a, ?) AND x BETWEEN ? AND ?",
		"SELECT * FROM t WHERE id = 'a' AND x IN [1, 2] AND y IS MISSING",
		"UPDATE t SET a = 1, b = b + 1, c = list_append(c, [1]) REMOVE d WHERE id = 'a'",
		"UPDATE t SET m['x'] = ? REMOVE m['y'] WHERE id = ? RETURNING ALL NEW *",
		"UPDATE t REMOVE x REMOVE c WHERE id = 'b'",
		"UPDATE t SET a = set_add(a, <<1, 2>>) WHERE id = 'a' AND x",
		"DELETE FROM t WHERE id = 'a' AND attribute_exists(x) RETURNING ALL OLD *",
		"INSERT INTO t VALUE {'id': 'a', 'n': 1, 'l': [1, {'k': ?}]}",
		"SELECT * FROM t.i WHERE id = 'a' ORDER BY k DESC",
		"SELECT DISTINCT a FROM t WHERE id = 'a'",
		"SELECT a AS z FROM t u /* c */ WHERE id = 'a'",
		"SELECT m[''] FROM t",
		"SELECT * FROM t WHERE id = 'a' AND foo(x)",
		"EXISTS(SELECT x FROM t WHERE id = 'a')",
	} {
		f.Add(src)
	}

	f.Fuzz(func(t *testing.T, src string) {
		stmt, err := partiql.Parse(src)
		if err != nil {
			return
		}

		params := make([]api.AttributeValue, min(stmt.Params, 8))
		for i := range params {
			s := "p" + strconv.Itoa(i)
			params[i] = api.AttributeValue{S: &s}
		}

		r := newRender(params)

		list, _, listErr := projection(stmt.Projection, r)
		cond, condErr := renderCond(stmt.Where, r)
		update, rank, updateErr := r.update(stmt.Actions)

		if listErr != nil || condErr != nil || updateErr != nil {
			return
		}

		text := strings.Join([]string{update, textOf(list), textOf(cond)}, " ")

		checkPlaceholders(t, src, text, r)

		var updateText *string
		if update != "" {
			updateText = &update
		}

		err = checkStatementExprs(r.attributeNames(), r.attributeValues(), cond, updateText, rank, 0)
		if err != nil && isSyntaxError(err) && regularStatement(stmt) {
			t.Fatalf("%q renders %q and %q, which the expression parser does not read: %v", src, textOf(cond), update, err)
		}
	})
}

// textOf is the text s points to, "" for none.
func textOf(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}

// checkPlaceholders fails when the #names and :values of text are not the names
// and values r handed back: one that is not there, or one that is not used.
func checkPlaceholders(t *testing.T, src, text string, r *render) {
	t.Helper()

	used := map[string]bool{}

	for _, ph := range namePlaceholder.FindAllString(text, -1) {
		_, ok := r.attributeNames()[ph]
		require.True(t, ok, "%q renders %q, which names %s, not in %v", src, text, ph, r.attributeNames())

		used[ph] = true
	}

	for ph := range r.attributeNames() {
		require.True(t, used[ph], "%q renders %q, which leaves %s unused", src, text, ph)
	}

	used = map[string]bool{}

	for _, ph := range valuePlaceholder.FindAllString(text, -1) {
		_, ok := r.attributeValues()[ph]
		require.True(t, ok, "%q renders %q, which holds %s, not in %v", src, text, ph, r.attributeValues())

		used[ph] = true
	}

	for ph := range r.attributeValues() {
		require.True(t, used[ph], "%q renders %q, which leaves %s unused", src, text, ph)
	}
}
