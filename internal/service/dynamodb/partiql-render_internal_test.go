package dynamodb

import (
	"regexp"
	"strconv"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/partiql"
)

// placeholder is a #name or :value in rendered expression text.
var placeholder = regexp.MustCompile(`[#:]n?v?[0-9]+`)

// FuzzPartiQLRender: a statement the parser accepts is rendered to the end,
// its WHERE as a condition, its actions as an update and its list as a
// projection, with an answer or an error and never a panic or a render that
// does not end (an unknown node kind is an error). Every #name a rendered
// text uses is one of the names the render handed back. The seed corpus is
// the f.Add statements below.
func FuzzPartiQLRender(f *testing.F) {
	for _, src := range []string{
		"SELECT * FROM t WHERE id = 'a'",
		"SELECT a, m['x'], l[0] FROM t WHERE id = 'a' AND (x > 1 OR NOT y = 2)",
		"SELECT * FROM t WHERE id = 'a' AND x = y + 1",
		"SELECT * FROM t WHERE id = ? AND begins_with(a, ?) AND x BETWEEN ? AND ?",
		"SELECT * FROM t WHERE id = 'a' AND x IN [1, 2] AND y IS MISSING",
		"UPDATE t SET a = 1, b = b + 1, c = list_append(c, [1]) REMOVE d WHERE id = 'a'",
		"UPDATE t SET m['x'] = ? REMOVE m['y'] WHERE id = ? RETURNING ALL NEW *",
		"UPDATE t REMOVE x, c WHERE id = 'b'",
		"UPDATE t SET a = set_add(a, <<1, 2>>) WHERE id = 'a' AND x",
		"DELETE FROM t WHERE id = 'a' AND attribute_exists(x) RETURNING ALL OLD *",
		"INSERT INTO t VALUE {'id': 'a', 'n': 1, 'l': [1, {'k': ?}]}",
		"SELECT * FROM t.i WHERE id = 'a' ORDER BY k DESC",
		"SELECT a AS z, DISTINCT FROM t u /* c */ WHERE id = 'a'",
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

		var texts []string

		cond, err := renderCond(stmt.Where, r)
		if err == nil && cond != nil {
			texts = append(texts, *cond)
		}

		update, _, err := r.update(stmt.Actions)
		if err == nil {
			texts = append(texts, update)
		}

		list, _, err := projection(stmt.Projection, r)
		if err == nil && list != nil {
			texts = append(texts, *list)
		}

		for _, text := range texts {
			for _, ph := range placeholder.FindAllString(text, -1) {
				if ph[0] != '#' {
					continue
				}

				if _, ok := r.attributeNames()[ph]; !ok {
					t.Fatalf("%q renders %q, which names %s, not in %v", src, text, ph, r.attributeNames())
				}
			}
		}
	})
}
