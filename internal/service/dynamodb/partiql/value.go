package partiql

import (
	"slices"
	"strconv"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
)

// MsgParamCount is the message for a statement that holds more ? than the
// request gives parameters.
const MsgParamCount = "Number of parameters in request and statement don't match."

const (
	msgUnsupportedType = "Unsupported data type: "
	msgBagType         = "Unsupported data type in Bag. DynamoDB only supports either numbers or strings in bags"
	msgEmptyBag        = "Empty bags are not supported"
	msgDuplicateKey    = "Duplicate keys in tuple. Key: "
	msgBagDuplicates   = "Input collection contains duplicates"
	rootKey            = "root"
)

// IsValue reports whether n is a value: a literal, a parameter, or a tuple,
// list or bag.
func IsValue(n *Node) bool {
	switch n.Kind {
	case KindLiteral, KindParam, KindTuple, KindList, KindBag:
		return true
	default:
		return false
	}
}

// Value is the attribute value n stands for, with params for its ?. under
// names where n stands in an INSERT's item, for messages: "" for the item
// itself, "root" for a value it holds, "root.k" for one below.
func Value(n *Node, params []api.AttributeValue, under string) (api.AttributeValue, error) {
	switch n.Kind {
	case KindLiteral:
		return literal(n.Literal), nil
	case KindParam:
		if n.Param >= len(params) {
			return api.AttributeValue{}, &Error{Message: MsgParamCount}
		}

		return params[n.Param], nil
	case KindTuple:
		return tupleValue(n, params, under)
	case KindList:
		list := make([]api.AttributeValue, len(n.Args))

		for i, a := range n.Args {
			v, err := Value(a, params, under+"."+strconv.Itoa(i))
			if err != nil {
				return api.AttributeValue{}, err
			}

			list[i] = v
		}

		return api.AttributeValue{L: list}, nil
	case KindBag:
		return bagValue(n, params, under)
	default:
		return api.AttributeValue{}, unsupported(n, under)
	}
}

// unsupported refuses n as a value: an INSERT's item names a path or MISSING.
// DynamoDB Local names the token at the item's top level and the node kind
// below it.
func unsupported(n *Node, under string) error {
	what := n.Text
	if under != rootKey && n.Kind == KindPath {
		what = "VariableReference"
	}

	return atError(msgUnsupportedType+what+" under key "+under, n.At)
}

func literal(l Literal) api.AttributeValue {
	switch l.Kind {
	case LitString:
		return api.AttributeValue{S: new(l.Text)}
	case LitNumber:
		return api.AttributeValue{N: new(l.Text)}
	case LitBool:
		return api.AttributeValue{BOOL: new(l.Bool)}
	default:
		return api.AttributeValue{NULL: new(true)}
	}
}

func tupleValue(n *Node, params []api.AttributeValue, under string) (api.AttributeValue, error) {
	m := make(map[string]api.AttributeValue, len(n.Keys))

	// An INSERT's item itself is under "", and DynamoDB Local names it and
	// the values it holds "root"; deeper values add their keys.
	display := under
	if display == "" {
		display = rootKey
	}

	for i, k := range n.Keys {
		if _, dup := m[k]; dup {
			return api.AttributeValue{}, atError(msgDuplicateKey+k+" under "+display, n.KeyAt[i])
		}

		next := rootKey
		if under != "" {
			next = under + "." + k
		}

		v, err := Value(n.Args[i], params, next)
		if err != nil {
			return api.AttributeValue{}, err
		}

		m[k] = v
	}

	return api.AttributeValue{M: m}, nil
}

// bagValue is a string or number set: every member a literal or parameter of
// one of the two types.
func bagValue(n *Node, params []api.AttributeValue, under string) (api.AttributeValue, error) {
	if len(n.Args) == 0 {
		return api.AttributeValue{}, atError(msgEmptyBag, n.At)
	}

	var out api.AttributeValue

	for i, a := range n.Args {
		v, err := Value(a, params, under+"."+strconv.Itoa(i))
		if err != nil {
			return api.AttributeValue{}, err
		}

		switch {
		case v.S != nil && out.NS == nil:
			out.SS = append(out.SS, *v.S)
		case v.N != nil && out.SS == nil:
			out.NS = append(out.NS, *v.N)
		default:
			return api.AttributeValue{}, &Error{Message: msgBagType}
		}
	}

	// DynamoDB Local refuses a repeated string with the number set's words
	// (verification/SQ-229 probe4); a repeated number is the value check's.
	if sorted := slices.Sorted(slices.Values(out.SS)); len(slices.Compact(sorted)) < len(out.SS) {
		return api.AttributeValue{}, &Error{Message: msgBagDuplicates}
	}

	return out, nil
}
