package dynamodb

import (
	"cmp"
	"slices"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/partiql"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// indexCut is where a SELECT's result stops following a path: at its first
// index step after the first that is not 0, or 0 when it has none. The image
// reads a path through the item its projection kept, where a list holds only
// the elements the projection named, so an element it names past the first
// has moved and its key and value are the list element's, whole and projected
// by what is left of the path: `l[2].z` is `l[2]`, the element with its `z`,
// where `l[0].z` is `z` (verification/SQ-439 probe4, probe5).
func indexCut(path partiql.Path) int {
	for i := 1; i < len(path); i++ {
		if path[i].IsIndex && path[i].Index != 0 {
			return i
		}
	}

	return 0
}

// cutPaths are the paths of a SELECT that stop at an index (indexCut), under
// the key each takes, which is the path to the index named by its last name and
// the indexes after it. Paths with one key, and one element, share a value:
// `l[1].p, l[1].q` is one `l[1]` with both.
type cutPaths struct {
	keys []string
	cuts map[string][]*cutPath
}

// cutPath is one element a key names: the element, and the parts of it the
// paths that name it ask for.
type cutPath struct {
	element partiql.Path
	below   []partiql.Path
}

// add records path, cut at i.
func (c *cutPaths) add(path partiql.Path, i int) {
	if c.cuts == nil {
		c.cuts = map[string][]*cutPath{}
	}

	key := path[:i+1].Alias()
	element := path[:i+1]

	elements, seen := c.cuts[key]
	if !seen {
		c.keys = append(c.keys, key)
	}

	cp := findElement(elements, element)
	if cp == nil {
		cp = &cutPath{element: element}
		c.cuts[key] = append(elements, cp)
	}

	cp.below = append(cp.below, path[i+1:])
}

// findElement is the cutPath of elements that names element, nil when none.
func findElement(elements []*cutPath, element partiql.Path) *cutPath {
	for _, cp := range elements {
		if slices.Equal(cp.element, element) {
			return cp
		}
	}

	return nil
}

// put adds to out the value of each key, the element item holds at its path
// with only what the paths below it name. Two paths through different lists
// can take one key (`m.l[1].p, n.l[1].q` are both `l[1]`), and a key holds one
// value. The image keeps one path by an order gostack does not reproduce
// (verification/SQ-470: no rule fits every statement probed); gostack keeps
// the first in name order that item holds, whole, with nothing of the others,
// so what is answered is always what a path of the statement names
// (docs/compat.md).
func (c *cutPaths) put(out, item value.Item) {
	for _, key := range c.keys {
		// An attribute the SELECT names by that very name (`"l[1]"`) holds the
		// key, as it does on the image and did before paths were cut.
		if _, named := out[key]; named {
			continue
		}

		elements := c.cuts[key]
		if len(elements) > 1 {
			slices.SortFunc(elements, func(a, b *cutPath) int { return comparePaths(a.element, b.element) })
		}

		for _, cp := range elements {
			element, ok := lookupPath(item, cp.element)
			if !ok {
				continue
			}

			if v, ok := projectBelow(element, cp.below); ok {
				out[key] = v

				break
			}
		}
	}
}

// comparePaths orders paths by their steps, names before indexes.
func comparePaths(a, b partiql.Path) int {
	return slices.CompareFunc(a, b, func(x, y partiql.Step) int {
		switch {
		case x.IsIndex && !y.IsIndex:
			return 1
		case !x.IsIndex && y.IsIndex:
			return -1
		default:
			return cmp.Or(strings.Compare(x.Name, y.Name), cmp.Compare(x.Index, y.Index))
		}
	})
}

// projectBelow is v with only the parts the paths below name, a list holding
// only the elements they name, as a ProjectionExpression keeps them; ok is
// false when they name nothing v holds. A path that is empty names v itself.
func projectBelow(v api.AttributeValue, below []partiql.Path) (api.AttributeValue, bool) {
	if slices.ContainsFunc(below, func(p partiql.Path) bool { return len(p) == 0 }) {
		return v, true
	}

	switch {
	case v.M != nil:
		return projectMap(v.M, below)
	case v.L != nil:
		return projectList(v.L, below)
	default:
		return api.AttributeValue{}, false
	}
}

// projectMap is projectBelow of a map.
func projectMap(m map[string]api.AttributeValue, below []partiql.Path) (api.AttributeValue, bool) {
	out := map[string]api.AttributeValue{}

	for _, path := range below {
		step := path[0]
		if step.IsIndex {
			continue
		}

		if _, done := out[step.Name]; done {
			continue
		}

		child, ok := m[step.Name]
		if !ok {
			continue
		}

		named := tails(below, func(s partiql.Step) bool { return !s.IsIndex && s.Name == step.Name })

		if v, ok := projectBelow(child, named); ok {
			out[step.Name] = v
		}
	}

	return api.AttributeValue{M: out}, len(out) > 0
}

// projectList is projectBelow of a list: the elements the paths name, in list
// order.
func projectList(l []api.AttributeValue, below []partiql.Path) (api.AttributeValue, bool) {
	var indexes []int

	for _, path := range below {
		if step := path[0]; step.IsIndex && step.Index < len(l) && !slices.Contains(indexes, step.Index) {
			indexes = append(indexes, step.Index)
		}
	}

	slices.Sort(indexes)

	var out []api.AttributeValue

	for _, i := range indexes {
		if v, ok := projectBelow(l[i], tails(below, func(s partiql.Step) bool { return s.IsIndex && s.Index == i })); ok {
			out = append(out, v)
		}
	}

	return api.AttributeValue{L: out}, len(out) > 0
}

// tails are what is left of the paths whose first step is one same accepts.
func tails(paths []partiql.Path, same func(partiql.Step) bool) []partiql.Path {
	var out []partiql.Path

	for _, path := range paths {
		if same(path[0]) {
			out = append(out, path[1:])
		}
	}

	return out
}
