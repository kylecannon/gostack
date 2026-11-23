package expression

import (
	"strconv"

	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The operator names DynamoDB's messages use for the comparators.
var comparators = map[tokKind]string{
	tokEQ: "=", tokNE: "<>", tokLT: "<", tokLE: "<=", tokGT: ">", tokGE: ">=",
}

// ParseCondition parses a ConditionExpression or FilterExpression, kind
// naming which, and checks it in DynamoDB's order: size, syntax and redundant
// parentheses, reserved words, operator count, then each operand and
// operator from the left with its placeholders substituted from subs. The
// placeholders it uses are recorded in used.
//
//nolint:ireturn // the parsed forms are the interfaces expression.go declares
func ParseCondition(kind Kind, src string, subs Substitutions, used map[string]struct{}) (Condition, error) {
	p, err := newParser(kind, src)
	if err != nil {
		return nil, err
	}

	return conditionFrom(p, subs, used, 0)
}

// ParseStatementCondition is ParseCondition for the WHERE of a PartiQL
// statement, which DynamoDB Local holds to no size of its own: a SELECT whose
// WHERE is six IN lists of 100 values renders to 4,172 characters and is
// answered, the statement's own 8,192 bytes bounding the text (SQ-452). The
// operator count and every other check apply to it as to any condition.
//
// The image counts the operators of the WHERE as written, the key equalities
// and the ANDs that join them included, where the statement compiles to a
// condition that holds none of them. unwritten is how many operators the
// statement wrote that src does not: they count toward the 300, and toward
// nothing else (SQ-454).
//
//nolint:ireturn // the parsed forms are the interfaces expression.go declares
func ParseStatementCondition(
	kind Kind, src string, subs Substitutions, used map[string]struct{}, unwritten int,
) (Condition, error) {
	return conditionFrom(newUnboundedParser(kind, src), subs, used, unwritten)
}

//nolint:ireturn // as ParseCondition
func conditionFrom(p *parser, subs Substitutions, used map[string]struct{}, unwritten int) (Condition, error) {
	c, r, err := parseChecked(p, subs, used, unwritten)
	if err != nil {
		return nil, err
	}

	return &condition{root: c, src: r.p.src}, nil
}

// parseChecked parses a condition grammar expression and runs the checks every
// condition-grammar member shares. unwritten operators count toward the limit
// with the ones src holds.
func parseChecked(
	p *parser, subs Substitutions, used map[string]struct{}, unwritten int,
) (*cond, *resolver, error) {
	c, err := p.parseCondition()
	if err != nil {
		return nil, nil, err
	}

	r := &resolver{p: p, subs: subs, used: used}

	err = walkOperands(c, r.checkReserved)
	if err != nil {
		return nil, nil, err
	}

	if n := countOperators(c) + unwritten; n > maxOperators {
		return nil, nil, r.fail(msgTooManyOperators)
	}

	err = r.resolveCond(c)
	if err != nil {
		return nil, nil, err
	}

	return c, r, nil
}

// walkOperands calls fn on each top-level operand of c in text order.
func walkOperands(c *cond, fn func(*operand) error) error {
	switch c.kind {
	case condNot:
		return walkOperands(c.sub[0], fn)
	case condAnd, condOr:
		err := walkOperands(c.sub[0], fn)
		if err != nil {
			return err
		}

		return walkOperands(c.sub[1], fn)
	case condFunc:
		return fn(c.fn)
	default:
		for _, o := range c.operands {
			err := fn(o)
			if err != nil {
				return err
			}
		}

		return nil
	}
}

// countOperators counts the operators and functions in c as DynamoDB's limit
// counts them; parentheses do not count.
func countOperators(c *cond) int {
	switch c.kind {
	case condNot:
		return 1 + countOperators(c.sub[0])
	case condAnd, condOr:
		return 1 + countOperators(c.sub[0]) + countOperators(c.sub[1])
	case condFunc:
		return countFuncs(c.fn)
	default:
		n := 1
		for _, o := range c.operands {
			n += countFuncs(o)
		}

		return n
	}
}

func countFuncs(o *operand) int {
	if o.kind != opFunc {
		return 0
	}

	n := 1
	for _, a := range o.args {
		n += countFuncs(a)
	}

	return n
}

// resolveCond resolves c's operands and checks each operator, depth first and
// from the left, so the first problem in the text is the one reported.
func (r *resolver) resolveCond(c *cond) error {
	switch c.kind {
	case condNot:
		return r.resolveCond(c.sub[0])
	case condAnd, condOr:
		err := r.resolveCond(c.sub[0])
		if err != nil {
			return err
		}

		return r.resolveCond(c.sub[1])
	case condFunc:
		return r.resolveFunc(c.fn, useCondition)
	default:
		return r.resolveLeaf(c)
	}
}

// resolveLeaf resolves a comparison, BETWEEN or IN and checks its operator.
func (r *resolver) resolveLeaf(c *cond) error {
	for _, o := range c.operands {
		err := r.resolveOperand(o)
		if err != nil {
			return err
		}
	}

	switch c.kind {
	case condCompare:
		return r.checkCompare(c)
	case condBetween:
		return r.checkBetween(c)
	default:
		err := r.distinct("IN", c.operands)
		if err != nil {
			return err
		}

		if n := len(c.operands) - 1; n > maxInOperands {
			return r.fail("The IN operator is provided with too many operands; number of operands: " + strconv.Itoa(n))
		}

		return nil
	}
}

func (r *resolver) checkCompare(c *cond) error {
	name := comparators[c.op]

	err := r.distinct(name, c.operands)
	if err != nil {
		return err
	}

	if c.op == tokEQ || c.op == tokNE {
		return nil
	}

	return r.scalarOperands(name, c.operands)
}

// scalarOperands refuses a value operand of an ordering operator that is not
// a string, number or binary.
func (r *resolver) scalarOperands(name string, ops []*operand) error {
	for _, o := range ops {
		if t := operandType(o); t != "" && !isScalar(t) {
			return r.wrongType(name, t)
		}
	}

	return nil
}

func isScalar(t string) bool {
	return t == value.TypeS || t == value.TypeN || t == value.TypeB
}

func (r *resolver) checkBetween(c *cond) error {
	const name = "BETWEEN"

	err := r.distinct(name, c.operands)
	if err != nil {
		return err
	}

	err = r.scalarOperands(name, c.operands)
	if err != nil {
		return err
	}

	low, high := c.operands[1], c.operands[2]
	if low.kind != opValue || high.kind != opValue {
		return nil
	}

	bounds := "lower bound operand: " + formatValue(low.raw) + ", upper bound operand: " + formatValue(high.raw)

	if value.Type(low.val) != value.Type(high.val) {
		return r.fail("The BETWEEN operator requires same data type for lower and upper bounds; " + bounds)
	}

	if value.Compare(low.val, high.val) > 0 {
		return r.fail("The BETWEEN operator requires upper bound to be greater than or equal to lower bound; " + bounds)
	}

	return nil
}

// condition is a parsed ConditionExpression or FilterExpression.
type condition struct {
	root *cond
	src  string
}

// Eval reports whether item meets the condition. It never fails: every check
// DynamoDB makes on an expression's form ran when it was parsed, and an
// operand of the wrong type at evaluation makes its comparison false.
func (c *condition) Eval(item value.Item) (bool, error) {
	return evalCond(c.root, item), nil
}

// String returns the expression's text.
func (c *condition) String() string { return c.src }
