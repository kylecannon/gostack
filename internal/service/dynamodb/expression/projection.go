package expression

import (
	"slices"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// projection is a parsed ProjectionExpression: its paths, and the same paths
// as a tree that Apply walks once per item.
type projection struct {
	paths []Path
	root  *projNode
}

// projNode is one step of the projected paths. A leaf takes the whole value
// under it; otherwise the value's map keys in names, or its list elements in
// indexes, are projected in turn.
type projNode struct {
	leaf    bool
	names   map[string]*projNode
	indexes map[int]*projNode
	order   []int // the keys of indexes, ascending
}

// ParseProjection parses a ProjectionExpression: document paths separated by
// commas, none of which may overlap another. It checks, in DynamoDB's order,
// size, syntax, reserved words, then each path's depth, indexes and names,
// then overlaps.
//
//nolint:ireturn // the parsed forms are the interfaces expression.go declares
func ParseProjection(src string, subs Substitutions, used map[string]struct{}) (Projection, error) {
	p, err := newParser(ProjectionExpression, src)
	if err != nil {
		return nil, err
	}

	ops, err := p.parsePathList()
	if err != nil {
		return nil, err
	}

	r := &resolver{p: p, subs: subs, used: used}

	for _, o := range ops {
		err = r.checkReserved(o)
		if err != nil {
			return nil, err
		}
	}

	pr := &projection{paths: make([]Path, len(ops)), root: &projNode{}}

	for i, o := range ops {
		err = r.resolvePath(o)
		if err != nil {
			return nil, err
		}

		pr.paths[i] = o.path
	}

	err = r.checkOverlap(pr.paths)
	if err != nil {
		return nil, err
	}

	for _, path := range pr.paths {
		pr.root.add(path)
	}

	return pr, nil
}

func (p *parser) parsePathList() ([]*operand, error) {
	var ops []*operand

	for {
		if k := p.peek().kind; k != tokIdent && k != tokName {
			return nil, p.syntaxError()
		}

		o, err := p.parsePath()
		if err != nil {
			return nil, err
		}

		ops = append(ops, o)

		switch p.peek().kind {
		case tokComma:
			p.next()
		case tokEOF:
			return ops, nil
		default:
			return nil, p.syntaxError()
		}
	}
}

// checkOverlap refuses two paths of which one is the other or leads into it.
func (r *resolver) checkOverlap(paths []Path) error {
	for j := 1; j < len(paths); j++ {
		for i := range j {
			if isPrefix(paths[i], paths[j]) || isPrefix(paths[j], paths[i]) {
				return r.fail("Two document paths overlap with each other; must remove or rewrite one of these paths; path one: " +
					formatPath(paths[i]) + ", path two: " + formatPath(paths[j]))
			}
		}
	}

	return nil
}

func isPrefix(prefix, p Path) bool {
	return len(prefix) <= len(p) && samePath(prefix, p[:len(prefix)])
}

func (n *projNode) add(path Path) {
	cur := n

	for _, e := range path {
		cur = cur.child(e)
	}

	cur.leaf = true
}

func (n *projNode) child(e PathElement) *projNode {
	if e.Name != "" {
		if n.names == nil {
			n.names = make(map[string]*projNode)
		}

		c, ok := n.names[e.Name]
		if !ok {
			c = &projNode{}
			n.names[e.Name] = c
		}

		return c
	}

	if n.indexes == nil {
		n.indexes = make(map[int]*projNode)
	}

	c, ok := n.indexes[e.Index]
	if !ok {
		c = &projNode{}
		n.indexes[e.Index] = c

		i, _ := slices.BinarySearch(n.order, e.Index)
		n.order = slices.Insert(n.order, i, e.Index)
	}

	return c
}

// Paths returns the paths in the order the expression lists them.
func (pr *projection) Paths() []Path { return pr.paths }

// Apply returns the projected parts of item. A path that is not there is left
// out, as is a map or list that would hold nothing; the projected elements of
// a list keep their order and close up. The result shares values with item,
// which it does not change.
func (pr *projection) Apply(item value.Item) value.Item {
	return pr.root.applyMap(item)
}

func (n *projNode) applyMap(m map[string]api.AttributeValue) map[string]api.AttributeValue {
	out := make(map[string]api.AttributeValue, len(n.names))

	for name, c := range n.names {
		v, ok := m[name]
		if !ok {
			continue
		}

		if r, ok := c.apply(&v); ok {
			out[name] = r
		}
	}

	return out
}

func (n *projNode) apply(v *api.AttributeValue) (api.AttributeValue, bool) {
	if n.leaf {
		return *v, true
	}

	if v.M != nil && n.names != nil {
		if m := n.applyMap(v.M); len(m) > 0 {
			return api.AttributeValue{M: m}, true
		}
	}

	if v.L != nil && n.indexes != nil {
		var l []api.AttributeValue

		for _, i := range n.order {
			if i >= len(v.L) {
				break
			}

			if r, ok := n.indexes[i].apply(&v.L[i]); ok {
				l = append(l, r)
			}
		}

		if len(l) > 0 {
			return api.AttributeValue{L: l}, true
		}
	}

	return api.AttributeValue{}, false
}
