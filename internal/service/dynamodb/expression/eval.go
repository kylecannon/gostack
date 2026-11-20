package expression

import (
	"bytes"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// evalCond evaluates c against item, which may be nil.
func evalCond(c *cond, item value.Item) bool {
	switch c.kind {
	case condNot:
		return !evalCond(c.sub[0], item)
	case condAnd:
		return evalCond(c.sub[0], item) && evalCond(c.sub[1], item)
	case condOr:
		return evalCond(c.sub[0], item) || evalCond(c.sub[1], item)
	case condFunc:
		return evalFunc(c.fn, item)
	case condCompare:
		a, b := evalOperand(c.operands[0], item), evalOperand(c.operands[1], item)

		return compare(c.op, a, b)
	case condBetween:
		return between(evalOperand(c.operands[0], item), evalOperand(c.operands[1], item), evalOperand(c.operands[2], item))
	default:
		v := evalOperand(c.operands[0], item)
		if v == nil {
			return false
		}

		for _, o := range c.operands[1:] {
			if w := evalOperand(o, item); w != nil && value.Equal(v, w) {
				return true
			}
		}

		return false
	}
}

// compare applies a comparator. A missing operand makes every comparator false
// but <>, which it makes true; so does a type mismatch.
func compare(op tokKind, a, b *api.AttributeValue) bool {
	if a == nil || b == nil {
		return op == tokNE
	}

	if op == tokEQ {
		return value.Equal(a, b)
	}

	if op == tokNE {
		return !value.Equal(a, b)
	}

	t := value.Type(a)
	if t != value.Type(b) || !isScalar(t) {
		return false
	}

	n := value.Compare(a, b)

	switch op {
	case tokLT:
		return n < 0
	case tokLE:
		return n <= 0
	case tokGT:
		return n > 0
	default:
		return n >= 0
	}
}

func between(v, low, high *api.AttributeValue) bool {
	if v == nil || low == nil || high == nil {
		return false
	}

	t := value.Type(v)
	if !isScalar(t) || t != value.Type(low) || t != value.Type(high) {
		return false
	}

	return value.Compare(low, v) <= 0 && value.Compare(v, high) <= 0
}

// evalOperand returns the value an operand has in item, or nil when it has
// none: a missing path, or size() of a value that has no size.
func evalOperand(o *operand, item value.Item) *api.AttributeValue {
	switch o.kind {
	case opValue:
		return o.val
	case opPath:
		return lookup(item, o.path)
	default:
		return sizeOf(evalOperand(o.args[0], item))
	}
}

// lookup returns the value at path in item, or nil.
func lookup(item value.Item, path Path) *api.AttributeValue {
	v, ok := item[path[0].Name]
	if !ok {
		return nil
	}

	cur := &v

	for _, e := range path[1:] {
		switch {
		case e.Name != "" && cur.M != nil:
			next, ok := cur.M[e.Name]
			if !ok {
				return nil
			}

			cur = &next
		case e.Name == "" && cur.L != nil && e.Index < len(cur.L):
			cur = &cur.L[e.Index]
		default:
			return nil
		}
	}

	return cur
}

// sizeOf is size(): a string's length in UTF-16 code units, as DynamoDB Local
// counts it, a binary's bytes, and the members of a set, map or list.
func sizeOf(v *api.AttributeValue) *api.AttributeValue {
	if v == nil {
		return nil
	}

	var n int

	switch value.Type(v) {
	case value.TypeS:
		n = len(utf16.Encode([]rune(*v.S)))
	case value.TypeB:
		n = len(v.B)
	case value.TypeSS:
		n = len(v.SS)
	case value.TypeNS:
		n = len(v.NS)
	case value.TypeBS:
		n = len(v.BS)
	case value.TypeM:
		n = len(v.M)
	case value.TypeL:
		n = len(v.L)
	default:
		return nil
	}

	s := strconv.Itoa(n)

	return &api.AttributeValue{N: &s}
}

// evalFunc evaluates a function that stands as a condition.
func evalFunc(o *operand, item value.Item) bool {
	a := evalOperand(o.args[0], item)

	switch o.fn {
	case fnAttributeExists:
		return a != nil
	case fnAttributeNotExists:
		return a == nil
	}

	b := evalOperand(o.args[1], item)
	if a == nil || b == nil {
		return false
	}

	switch o.fn {
	case fnAttributeType:
		return value.Type(a) == *b.S
	case fnBeginsWith:
		return beginsWith(a, b)
	default:
		return contains(a, b)
	}
}

func beginsWith(a, b *api.AttributeValue) bool {
	switch {
	case a.S != nil && b.S != nil:
		return strings.HasPrefix(*a.S, *b.S)
	case a.B != nil && b.B != nil:
		return bytes.HasPrefix(a.B, b.B)
	default:
		return false
	}
}

// contains is contains(): a substring of a string, a subsequence of a binary,
// a member of a set of the operand's type, or an element of a list.
func contains(a, b *api.AttributeValue) bool {
	switch {
	case a.S != nil && b.S != nil:
		return strings.Contains(*a.S, *b.S)
	case a.B != nil && b.B != nil:
		return bytes.Contains(a.B, b.B)
	case a.SS != nil && b.S != nil:
		return slices.Contains(a.SS, *b.S)
	case a.NS != nil && b.N != nil:
		return slices.Contains(a.NS, *b.N)
	case a.BS != nil && b.B != nil:
		return slices.ContainsFunc(a.BS, func(e []byte) bool { return bytes.Equal(e, b.B) })
	case a.L != nil:
		return slices.ContainsFunc(a.L, func(e api.AttributeValue) bool { return value.Equal(&e, b) })
	default:
		return false
	}
}
