package dynamodb

import (
	"slices"

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
	cuts map[string]*cutPath
}

// cutPath is one key of cutPaths: the element it names, and the parts of it the
// paths that share the key ask for.
type cutPath struct {
	element partiql.Path
	below   []partiql.Path
}

// add records path, cut at i.
func (c *cutPaths) add(path partiql.Path, i int) {
	if c.cuts == nil {
		c.cuts = map[string]*cutPath{}
	}

	key := path[:i+1].Alias()

	cp, ok := c.cuts[key]
	if !ok {
		cp = &cutPath{element: path[:i+1]}
		c.cuts[key] = cp
		c.keys = append(c.keys, key)
	}

	cp.below = append(cp.below, path[i+1:])
}

// put adds to out the value of each key, the element item holds at its path
// with only what the paths below it name.
func (c *cutPaths) put(out, item value.Item) {
	for _, key := range c.keys {
		cp := c.cuts[key]

		element, ok := lookupPath(item, cp.element)
		if !ok {
			continue
		}

		if v, ok := projectBelow(element, cp.below); ok {
			out[key] = v
		}
	}
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
