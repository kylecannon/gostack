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
	msgKeyLength         = "Conditions can be of length 1 or 2 only"
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

// maxKeyConditions is the most conditions a key condition has: one on the hash
// attribute and one on the range attribute.
const maxKeyConditions = 2

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

// EmptyKey names the message DynamoDB refuses an empty key value with, or ""
// for a value that is not empty. Fit asks it of the values of a key condition
// in the order DynamoDB checks them.
type EmptyKey func(attr string, v *api.AttributeValue) string

// KeyText is a KeyConditionExpression that has parsed and had its placeholders
// resolved, but is not yet held to the key schema: DynamoDB checks the
// placeholders of the whole request between the two.
type KeyText struct {
	c *cond
}

// ParseKeyConditionText parses a KeyConditionExpression and makes the checks
// every condition makes.
func ParseKeyConditionText(src string, subs Substitutions, used map[string]struct{}) (*KeyText, error) {
	c, _, err := parseChecked(KeyConditionExpression, src, subs, used)
	if err != nil {
		return nil, err
	}

	return &KeyText{c: c}, nil
}

// Shape checks the expression's operators, one key attribute per condition,
// and one condition per key, and returns its conditions for Fit.
func (k *KeyText) Shape() (*KeyShape, error) {
	var parts []keyPart

	err := collectKeyParts(k.c, &parts)
	if err != nil {
		return nil, err
	}

	err = checkOnePerKey(parts)
	if err != nil {
		return nil, err
	}

	return &KeyShape{parts: parts}, nil
}

// KeyShape is the conditions of a key condition whose shape is checked.
type KeyShape struct {
	parts []keyPart
}

// Fit holds the conditions to key, the key schema of the table or index the
// query reads, in DynamoDB's order: at most two conditions, none on a second
// attribute of a hash-only key, the hash attribute present, its values' type,
// equality, and its value not empty, then the range attribute named, its
// values' type, and its values not empty. empty may be nil.
//
//nolint:ireturn // the parsed form is the interface expression.go declares
func (s *KeyShape) Fit(key table.KeySchema, empty EmptyKey) (KeyCondition, error) {
	kc, err := fitKeyParts(s.parts, key, empty)
	if err != nil {
		return nil, err
	}

	return kc, nil
}

// ParseKeyCondition parses a KeyConditionExpression against key, the key
// schema of the table or index the query reads, in one step: the checks every
// condition makes, then DynamoDB's key checks (see Fit), the empty value
// aside.
//
//nolint:ireturn // the parsed forms are the interfaces expression.go declares
func ParseKeyCondition(
	src string, key table.KeySchema, subs Substitutions, used map[string]struct{},
) (KeyCondition, error) {
	text, err := ParseKeyConditionText(src, subs, used)
	if err != nil {
		return nil, err
	}

	shape, err := text.Shape()
	if err != nil {
		return nil, err
	}

	return shape.Fit(key, nil)
}

// checkOnePerKey refuses two conditions on one attribute.
func checkOnePerKey(parts []keyPart) error {
	seen := make(map[string]bool, len(parts))
	for _, p := range parts {
		if seen[p.name] {
			return value.Invalid(msgKeyTwice)
		}

		seen[p.name] = true
	}

	return nil
}

// fitKeyParts is Fit: DynamoDB Local's order for the checks of a key condition
// against a key schema (probed, SQ-331).
func fitKeyParts(parts []keyPart, key table.KeySchema, empty EmptyKey) (*keyCondition, error) {
	switch {
	case len(parts) > maxKeyConditions:
		return nil, value.Invalid(msgKeyLength)
	case len(parts) == maxKeyConditions && !key.HasRange():
		return nil, value.Invalid(msgKeyNotSupported)
	}

	kc := &keyCondition{}

	for _, p := range parts {
		if p.name == key.Hash.Name {
			kc.hash = p
		}
	}

	if kc.hash.name == "" {
		return nil, value.Invalid(msgKeyMissed)
	}

	err := checkKeyPartType(kc.hash, key.Hash.Type)
	if err == nil && kc.hash.op != table.SortEQ {
		err = value.Invalid(msgKeyNotSupported)
	}

	if err == nil {
		err = checkKeyPartEmpty(kc.hash, empty)
	}

	if err != nil {
		return nil, err
	}

	for _, p := range parts {
		if p.name == key.Hash.Name {
			continue
		}

		if p.name != key.Range.Name {
			return nil, value.Invalid(msgKeyMissed)
		}

		kc.rng, kc.isRng = p, true

		err = checkKeyPartType(p, key.Range.Type)
		if err == nil {
			err = checkKeyPartEmpty(p, empty)
		}

		if err != nil {
			return nil, err
		}
	}

	return kc, nil
}

// checkKeyPartType refuses a condition whose values are not of the key
// attribute's type.
func checkKeyPartType(p keyPart, typ string) error {
	if !partHasType(p, typ) {
		return value.Invalid(msgKeyTypeMismatch)
	}

	return nil
}

// checkKeyPartEmpty refuses a condition with an empty key value; empty may be
// nil.
func checkKeyPartEmpty(p keyPart, empty EmptyKey) error {
	if empty == nil {
		return nil
	}

	for i := range p.values {
		msg := empty(p.name, &p.values[i])
		if msg != "" {
			return value.Invalid(msg)
		}
	}

	return nil
}

// buildKeyCondition is the legacy KeyConditions' key checks: one condition
// per key, then Fit without the empty value, which a legacy Query checks
// later.
func buildKeyCondition(parts []keyPart, key table.KeySchema) (*keyCondition, error) {
	err := checkOnePerKey(parts)
	if err != nil {
		return nil, err
	}

	return fitKeyParts(parts, key, nil)
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
