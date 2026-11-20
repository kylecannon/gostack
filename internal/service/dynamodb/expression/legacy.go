package expression

import (
	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/table"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The legacy ComparisonOperators of Condition and ExpectedAttributeValue.
const (
	LegacyEQ          = "EQ"
	LegacyNE          = "NE"
	LegacyLE          = "LE"
	LegacyLT          = "LT"
	LegacyGE          = "GE"
	LegacyGT          = "GT"
	LegacyNotNull     = "NOT_NULL"
	LegacyNull        = "NULL"
	LegacyContains    = "CONTAINS"
	LegacyNotContains = "NOT_CONTAINS"
	LegacyBeginsWith  = "BEGINS_WITH"
	LegacyIn          = "IN"
	LegacyBetween     = "BETWEEN"
)

// The messages DynamoDB Local answers for a legacy KeyConditions that is
// well formed but cannot be a key condition, as the LocalStack image returns
// them (verification/SQ-311 r2, r3). The ones a key condition expression
// shares are in keycondition.go.
const (
	msgLegacyKeyNotIndexable = "Attempted conditional constraint is not an indexable operation"
	msgLegacyHashNotScalar   = "Only scalar types can be used as Hash Key values"
	msgLegacyRangeNotScalar  = "Only scalar types can be used as Range Key values"
)

// LegacyTest is one legacy comparison: the attribute named Name against
// Values, as the ComparisonOperator Op says. The caller has checked Op, the
// number of Values and their types.
type LegacyTest struct {
	Name   string
	Op     string
	Values []api.AttributeValue
}

// legacyComparators are the legacy operators that a comparison node evaluates.
var legacyComparators = map[string]tokKind{
	LegacyEQ: tokEQ, LegacyNE: tokNE, LegacyLE: tokLE, LegacyLT: tokLT, LegacyGE: tokGE, LegacyGT: tokGT,
}

// Legacy translates the legacy conditions of a request's ScanFilter,
// QueryFilter or Expected into the parsed form a condition expression
// parses to, so one evaluator serves both: all of tests must hold, or any one
// when anyOf is set. It returns nil for no tests. Every Values entry must be
// a validated value.
//
//nolint:ireturn // the parsed form is the package's interface
func Legacy(tests []LegacyTest, anyOf bool) Condition {
	var root *cond

	for i := range tests {
		c := legacyCond(&tests[i])

		switch {
		case root == nil:
			root = c
		case anyOf:
			root = &cond{kind: condOr, sub: [2]*cond{root, c}}
		default:
			root = &cond{kind: condAnd, sub: [2]*cond{root, c}}
		}
	}

	if root == nil {
		return nil
	}

	return &condition{root: root}
}

func legacyCond(t *LegacyTest) *cond {
	path := &operand{kind: opPath, path: Path{{Name: t.Name}}}

	operands := make([]*operand, 0, 1+len(t.Values))
	operands = append(operands, path)

	for i := range t.Values {
		v := t.Values[i]
		operands = append(operands, &operand{kind: opValue, val: &v, raw: &v})
	}

	if op, ok := legacyComparators[t.Op]; ok {
		return &cond{kind: condCompare, op: op, operands: operands}
	}

	switch t.Op {
	case LegacyNull:
		return legacyFunc(fnAttributeNotExists, path)
	case LegacyNotNull:
		return legacyFunc(fnAttributeExists, path)
	case LegacyContains:
		return legacyFunc(fnContains, operands...)
	case LegacyNotContains:
		// An attribute the item lacks matches neither CONTAINS nor
		// NOT_CONTAINS (probed).
		return &cond{kind: condAnd, sub: [2]*cond{
			legacyFunc(fnAttributeExists, path),
			{kind: condNot, sub: [2]*cond{legacyFunc(fnContains, operands...)}},
		}}
	case LegacyBeginsWith:
		return legacyFunc(fnBeginsWith, operands...)
	case LegacyBetween:
		return &cond{kind: condBetween, operands: operands}
	default:
		return &cond{kind: condIn, operands: operands}
	}
}

func legacyFunc(name string, args ...*operand) *cond {
	return &cond{kind: condFunc, fn: &operand{kind: opFunc, fn: name, args: args}}
}

// legacySortOps are the legacy operators a range key can be read by.
var legacySortOps = map[string]table.SortOp{
	LegacyEQ: table.SortEQ, LegacyLT: table.SortLT, LegacyLE: table.SortLE, LegacyGT: table.SortGT,
	LegacyGE: table.SortGE, LegacyBetween: table.SortBetween, LegacyBeginsWith: table.SortBeginsWith,
}

// LegacyKeyCondition translates a legacy Query's KeyConditions, whose
// operators, value counts and value types the caller has checked, into the
// parsed form of a KeyConditionExpression for the key schema key of the table
// or index read, and makes the key checks DynamoDB Local does: NULL and
// NOT_NULL are no key condition, then the shared schema checks (the hash
// attribute present, no other attribute, the values' types, equality on the
// hash key), then an operator a range key cannot be read by.
//
//nolint:ireturn // the parsed form is the package's interface
func LegacyKeyCondition(tests []LegacyTest, key table.KeySchema) (KeyCondition, error) {
	parts := make([]keyPart, 0, len(tests))

	for i := range tests {
		t := &tests[i]

		if t.Op == LegacyNull || t.Op == LegacyNotNull {
			if t.Name == key.Hash.Name {
				return nil, value.Invalid(msgLegacyHashNotScalar)
			}

			if key.HasRange() && t.Name == key.Range.Name {
				return nil, value.Invalid(msgLegacyRangeNotScalar)
			}
		}

		// SortAny marks an operator no sort condition expresses: the shared
		// check refuses it on the hash key, the one below on the range key.
		parts = append(parts, keyPart{name: t.Name, op: legacySortOps[t.Op], values: t.Values})
	}

	kc, err := buildKeyCondition(parts, key)
	if err != nil {
		return nil, err
	}

	if kc.isRng && kc.rng.op == table.SortAny {
		for i := range tests {
			switch tests[i].Op {
			case LegacyIn, LegacyContains, LegacyNotContains:
				if tests[i].Name == kc.rng.name {
					return nil, value.Invalid(msgLegacyKeyNotIndexable)
				}
			default:
			}
		}

		return nil, value.Invalid(msgKeyNotSupported)
	}

	return kc, nil
}
