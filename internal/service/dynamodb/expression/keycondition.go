package expression

import (
	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/table"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The messages DynamoDB Local answers for a key condition that parses but does
// not fit the key schema. Unlike the others they carry no "Invalid
// KeyConditionExpression:" prefix. Each was probed against
// gostack-parity-localstack:8b9a79f, except msgBeginsWithFirst, which mirrors
// the BETWEEN message.
const (
	msgKeyNotSupported   = "Query key condition not supported"
	msgKeyMissed         = "Query condition missed key schema element"
	msgKeyTwice          = "KeyConditionExpressions must only contain one condition per key"
	msgKeyNested         = "KeyConditionExpressions cannot contain nested operations"
	msgKeyTypeMismatch   = "One or more parameter values were invalid: Condition parameter type does not match schema type"
	msgKeyBadOperator    = "Invalid operator used in KeyConditionExpression: "
	msgKeyBadCondition   = "Invalid condition in KeyConditionExpression: "
	msgKeyNoAttribute    = msgKeyBadCondition + "No key attribute specified"
	msgKeyTwoAttributes  = msgKeyBadCondition + "Multiple attribute names used in one condition"
	msgBetweenFirst      = msgKeyBadCondition + "BETWEEN operator must have the key attribute as its first operand"
	msgBeginsWithFirst   = msgKeyBadCondition + "begins_with operator must have the key attribute as its first operand"
	msgKeyNotScalarStart = "Key attributes must be scalars; " +
		"list random access '[]' and map lookup '.' are not allowed: Key: "
)

// keyPart is one condition of a key condition: an attribute and the
// comparison its value must meet.
type keyPart struct {
	name   string
	op     table.SortOp
	values []api.AttributeValue
}

// keyCondition is a parsed KeyConditionExpression.
type keyCondition struct {
	hash  keyPart
	rng   keyPart
	isRng bool
}

// ParseKeyCondition parses a KeyConditionExpression against key, the key
// schema of the table or index the query reads. It makes the checks every
// condition makes, then DynamoDB's key checks: the operators, one key
// attribute per condition, one condition per key, the hash attribute present
// and no other attribute, the values' types, and equality on the hash key.
//
//nolint:ireturn // the parsed forms are the interfaces expression.go declares
func ParseKeyCondition(
	src string, key table.KeySchema, subs Substitutions, used map[string]struct{},
) (KeyCondition, error) {
	c, _, err := parseChecked(KeyConditionExpression, src, subs, used)
	if err != nil {
		return nil, err
	}

	var parts []keyPart
	err = collectKeyParts(c, &parts)
	if err != nil {
		return nil, err
	}

	return buildKeyCondition(parts, key)
}

func buildKeyCondition(parts []keyPart, key table.KeySchema) (*keyCondition, error) {
	seen := make(map[string]bool, len(parts))
	for _, p := range parts {
		if seen[p.name] {
			return nil, value.Invalid(msgKeyTwice)
		}

		seen[p.name] = true
	}

	kc := &keyCondition{}

	for _, p := range parts {
		switch {
		case p.name == key.Hash.Name:
			kc.hash = p
		case key.HasRange() && p.name == key.Range.Name:
			kc.rng, kc.isRng = p, true
		default:
			return nil, value.Invalid(msgKeyMissed)
		}
	}

	if !seen[key.Hash.Name] {
		return nil, value.Invalid(msgKeyMissed)
	}

	if !partHasType(kc.hash, key.Hash.Type) || (kc.isRng && !partHasType(kc.rng, key.Range.Type)) {
		return nil, value.Invalid(msgKeyTypeMismatch)
	}

	if kc.hash.op != table.SortEQ {
		return nil, value.Invalid(msgKeyNotSupported)
	}

	return kc, nil
}

func partHasType(p keyPart, t string) bool {
	for i := range p.values {
		if value.Type(&p.values[i]) != t {
			return false
		}
	}

	return true
}

// The sort operations of the comparators, and of each comparator with its
// operands swapped.
var (
	sortOps = map[tokKind]table.SortOp{
		tokEQ: table.SortEQ, tokLT: table.SortLT, tokLE: table.SortLE, tokGT: table.SortGT, tokGE: table.SortGE,
	}
	swappedSortOps = map[tokKind]table.SortOp{
		tokEQ: table.SortEQ, tokLT: table.SortGT, tokLE: table.SortGE, tokGT: table.SortLT, tokGE: table.SortLE,
	}
)

// collectKeyParts walks c's conjunction, refusing any operator a key
// condition does not take.
func collectKeyParts(c *cond, parts *[]keyPart) error {
	switch c.kind {
	case condAnd:
		err := collectKeyParts(c.sub[0], parts)
		if err != nil {
			return err
		}

		return collectKeyParts(c.sub[1], parts)
	case condOr:
		return value.Invalid(msgKeyBadOperator + "OR")
	case condNot:
		return value.Invalid(msgKeyBadOperator + "NOT")
	case condIn:
		return value.Invalid(msgKeyBadOperator + "IN")
	case condFunc:
		if c.fn.fn != fnBeginsWith {
			return value.Invalid(msgKeyBadOperator + c.fn.fn)
		}

		return addKeyPart(parts, c.fn.args, table.SortBeginsWith, table.SortBeginsWith, msgBeginsWithFirst)
	case condBetween:
		return addKeyPart(parts, c.operands, table.SortBetween, table.SortBetween, msgBetweenFirst)
	default:
		if c.op == tokNE {
			return value.Invalid(msgKeyBadOperator + "<>")
		}

		return addKeyPart(parts, c.operands, sortOps[c.op], swappedSortOps[c.op], "")
	}
}

// addKeyPart adds the condition over ops, whose one path names the key
// attribute. op applies when the path comes first and swapped when it does
// not; firstOnly, when set, refuses a path that does not come first.
func addKeyPart(parts *[]keyPart, ops []*operand, op, swapped table.SortOp, firstOnly string) error {
	at := -1

	for i, o := range ops {
		if o.kind == opFunc {
			return value.Invalid(msgKeyNested)
		}

		if o.kind != opPath {
			continue
		}

		if at >= 0 {
			return value.Invalid(msgKeyTwoAttributes)
		}

		at = i
	}

	if at < 0 {
		return value.Invalid(msgKeyNoAttribute)
	}

	path := ops[at].path
	if len(path) > 1 {
		return value.Invalid(msgKeyNotScalarStart + path[0].Name)
	}

	if at > 0 {
		if firstOnly != "" {
			return value.Invalid(firstOnly)
		}

		op = swapped
	}

	p := keyPart{name: path[0].Name, op: op}

	for i, o := range ops {
		if i != at {
			p.values = append(p.values, *o.val)
		}
	}

	*parts = append(*parts, p)

	return nil
}

// Hash returns the hash attribute's name and the value it must equal.
func (k *keyCondition) Hash() (string, api.AttributeValue) {
	return k.hash.name, k.hash.values[0]
}

// Range returns the range attribute's name and the comparison on it.
func (k *keyCondition) Range() (string, table.SortCondition, bool) {
	if !k.isRng {
		return "", table.SortCondition{}, false
	}

	return k.rng.name, table.SortCondition{Op: k.rng.op, Values: k.rng.values}, true
}
