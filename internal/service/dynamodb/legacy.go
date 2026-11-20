package dynamodb

import (
	"fmt"
	"maps"
	"math"
	"slices"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/kylecannon/gostack/internal/service/dynamodb/table"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The messages DynamoDB Local answers for the legacy condition parameters
// (KeyConditions, QueryFilter, ScanFilter, Expected, ConditionalOperator) it
// refuses, as the LocalStack image returns them (verification/SQ-311 r1 to r3).
const (
	msgInvalid         = "One or more parameter values were invalid: "
	msgNoComparison    = "Comparison type does not exist in DynamoDB"
	msgBadArgCount     = msgInvalid + "Invalid number of argument(s) for the %s ComparisonOperator"
	msgBadOperandType  = msgInvalid + "ComparisonOperator %s is not valid for %s AttributeValue type"
	msgMixedOperands   = msgInvalid + "AttributeValues inside AttributeValueList must be of same type"
	msgBetweenReversed = "The BETWEEN condition was provided a range where the lower bound is greater than the upper bound"
	msgCoAlone         = "ConditionalOperator cannot be used without Filter or Expected"
	msgCoSingle        = "ConditionalOperator can only be used when Filter or Expected has two or more elements"
	msgConditionsNone  = "Conditions must be at least length 1"
	msgConditionsMany  = "Conditions can be of length 1 or 2 only"
	msgQueryFilterKey  = "QueryFilter can only contain non-primary key attributes: Primary key attribute: "
	msgNullExprs       = "FilterExpression and KeyConditionExpression are null"

	msgExpectedBoth     = msgInvalid + "Value and AttributeValueList cannot be used together for Attribute: "
	msgExpectedExists   = msgInvalid + "Exists and ComparisonOperator cannot be used together for Attribute: "
	msgExpectedListOnly = msgInvalid + "AttributeValueList can only be used with a ComparisonOperator for Attribute: "
	msgExpectedNoValue  = msgInvalid +
		"Value or AttributeValueList must be used with ComparisonOperator: %s for Attribute: %s"
	msgExistsNull  = msgInvalid + "Value must be provided when Exists is null for Attribute: "
	msgExistsTrue  = msgInvalid + "Value must be provided when Exists is true for Attribute: "
	msgExistsFalse = msgInvalid + "Value cannot be used when Exists is false for Attribute: "
)

// The request parameters the legacy messages name.
const (
	paramScanFilter       = "ScanFilter"
	paramQueryFilter      = "QueryFilter"
	paramKeyConditions    = "KeyConditions"
	paramConditionalOp    = "ConditionalOperator"
	paramExpected         = "Expected"
	paramAttributeUpdates = "AttributeUpdates"
	paramUpdate           = "UpdateExpression"
	paramCondition        = "ConditionExpression"
)

// legacyOp is what a ComparisonOperator takes: how many values, and of which
// types, nil for any.
type legacyOp struct {
	minArgs, maxArgs int
	types            []string
}

var (
	typesScalar = []string{value.TypeS, value.TypeN, value.TypeB}
	typesText   = []string{value.TypeS, value.TypeB}
)

var legacyOps = map[string]legacyOp{
	expression.LegacyEQ:          {1, 1, nil},
	expression.LegacyNE:          {1, 1, nil},
	expression.LegacyLE:          {1, 1, typesScalar},
	expression.LegacyLT:          {1, 1, typesScalar},
	expression.LegacyGE:          {1, 1, typesScalar},
	expression.LegacyGT:          {1, 1, typesScalar},
	expression.LegacyNull:        {0, 0, nil},
	expression.LegacyNotNull:     {0, 0, nil},
	expression.LegacyContains:    {1, 1, typesScalar},
	expression.LegacyNotContains: {1, 1, typesScalar},
	expression.LegacyBeginsWith:  {1, 1, typesText},
	expression.LegacyIn:          {1, math.MaxInt, typesScalar},
	expression.LegacyBetween:     {2, 2, typesScalar},
}

// legacyTest checks one legacy condition, a ComparisonOperator and its
// AttributeValueList on the attribute name, and returns it ready to
// translate. Its values are validated and made canonical.
func legacyTest(name string, op *string, values []api.AttributeValue) (expression.LegacyTest, error) {
	if name == "" {
		return expression.LegacyTest{}, value.Invalid(msgAttrEmpty)
	}

	spec, ok := legacyOps[stringOf(op)]
	if !ok {
		return expression.LegacyTest{}, value.Invalid(msgNoComparison)
	}

	values = slices.Clone(values)

	for i := range values {
		err := value.Validate(&values[i])
		if err != nil {
			return expression.LegacyTest{}, fmt.Errorf("condition value: %w", err)
		}
	}

	err := checkLegacyValues(*op, &spec, values)
	if err != nil {
		return expression.LegacyTest{}, err
	}

	return expression.LegacyTest{Name: name, Op: *op, Values: values}, nil
}

func stringOf(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}

// checkLegacyValues checks the number, types and order of an operator's values.
func checkLegacyValues(op string, spec *legacyOp, values []api.AttributeValue) error {
	if len(values) < spec.minArgs || len(values) > spec.maxArgs {
		return value.Invalid(fmt.Sprintf(msgBadArgCount, op))
	}

	for i := range values {
		t := value.Type(&values[i])
		if spec.types != nil && !slices.Contains(spec.types, t) {
			return value.Invalid(fmt.Sprintf(msgBadOperandType, op, t))
		}

		if t != value.Type(&values[0]) {
			return value.Invalid(msgMixedOperands)
		}
	}

	if op == expression.LegacyBetween && value.Compare(&values[0], &values[1]) > 0 {
		return value.Invalid(msgBetweenReversed)
	}

	return nil
}

// legacyTests checks every condition of a ScanFilter, QueryFilter or
// KeyConditions, in attribute name order: the image reports the first in the
// request's order, which the decoded map does not keep.
func legacyTests(conds map[string]api.Condition) ([]expression.LegacyTest, error) {
	tests := make([]expression.LegacyTest, 0, len(conds))

	for _, name := range slices.Sorted(maps.Keys(conds)) {
		c := conds[name]

		t, err := legacyTest(name, c.ComparisonOperator, c.AttributeValueList)
		if err != nil {
			return nil, err
		}

		tests = append(tests, t)
	}

	return tests, nil
}

// legacyOperator checks ConditionalOperator against the n conditions it joins
// and reports whether it makes them any-of (OR) and not all-of (AND).
func legacyOperator(op *string, n int) (bool, error) {
	if op == nil {
		return false, nil
	}

	switch {
	case n == 0:
		return false, value.Invalid(msgCoAlone)
	case n == 1:
		return false, value.Invalid(msgCoSingle)
	case *op == "OR":
		return true, nil
	case *op == "AND":
		return false, nil
	default:
		return false, enumError("conditionalOperator", *op, "OR, AND")
	}
}

// legacyFilter turns a ScanFilter or QueryFilter and its ConditionalOperator
// into a condition, nil when there is no filter.
//
//nolint:ireturn // the parsed form is the expression package's interface
func legacyFilter(conds map[string]api.Condition, op *string) (expression.Condition, error) {
	anyOf, err := legacyOperator(op, len(conds))
	if err != nil {
		return nil, err
	}

	tests, err := legacyTests(conds)
	if err != nil {
		return nil, err
	}

	return expression.Legacy(tests, anyOf), nil
}

// legacyKey is a Query's legacy KeyConditions, and its QueryFilter, checked
// and translated.
type legacyKey struct {
	key    expression.KeyCondition
	filter expression.Condition
}

// legacyQuery checks a Query's KeyConditions, then its QueryFilter and
// ConditionalOperator, against the key schema of what it reads: DynamoDB
// Local's order. The QueryFilter may not name a key attribute of it.
func legacyQuery(in *api.QueryInput, schema table.KeySchema) (*legacyKey, error) {
	switch {
	case len(in.KeyConditions) == 0:
		return nil, value.Invalid(msgConditionsNone)
	case len(in.KeyConditions) > 2: //nolint:mnd // a hash and a range key
		return nil, value.Invalid(msgConditionsMany)
	}

	tests, err := legacyTests(in.KeyConditions)
	if err != nil {
		return nil, err
	}

	key, err := expression.LegacyKeyCondition(tests, schema)
	if err != nil {
		return nil, fmt.Errorf("key conditions: %w", err)
	}

	filter, err := legacyFilter(in.QueryFilter, in.ConditionalOperator)
	if err != nil {
		return nil, err
	}

	for _, name := range slices.Sorted(maps.Keys(in.QueryFilter)) {
		if name == schema.Hash.Name || (schema.HasRange() && name == schema.Range.Name) {
			return nil, value.Invalid(msgQueryFilterKey + name)
		}
	}

	return &legacyKey{key: key, filter: filter}, nil
}

// expectedTests checks every attribute of a write's Expected, in attribute
// name order, and returns them ready to translate. An attribute is expected
// by Value (with Exists, true unless it says otherwise) or by a
// ComparisonOperator over its values, never both.
func expectedTests(expected map[string]api.ExpectedAttributeValue) ([]expression.LegacyTest, error) {
	tests := make([]expression.LegacyTest, 0, len(expected))

	for _, name := range slices.Sorted(maps.Keys(expected)) {
		ev := expected[name]

		t, err := legacyExpectedTest(name, &ev)
		if err != nil {
			return nil, err
		}

		tests = append(tests, t)
	}

	return tests, nil
}

// expectedCondition checks a write's ConditionalOperator against the n tests
// of its Expected and translates them into a condition on the stored item,
// nil when there are neither. DynamoDB Local checks the operator after the
// entries of Expected, and a write's ReturnValuesOnConditionCheckFailure
// between them.
//
//nolint:ireturn // the parsed form is the expression package's interface
func expectedCondition(tests []expression.LegacyTest, op *string) (expression.Condition, error) {
	if len(tests) == 0 && op == nil {
		return nil, nil //nolint:nilnil // no legacy condition
	}

	anyOf, err := legacyOperator(op, len(tests))
	if err != nil {
		return nil, err
	}

	return expression.Legacy(tests, anyOf), nil
}

func legacyExpectedTest(name string, ev *api.ExpectedAttributeValue) (expression.LegacyTest, error) {
	if name == "" {
		return expression.LegacyTest{}, value.Invalid(msgAttrEmpty)
	}

	if ev.ComparisonOperator != nil {
		return legacyExpectedCompare(name, ev)
	}

	switch {
	case len(ev.AttributeValueList) > 0 && ev.Value != nil:
		return expression.LegacyTest{}, value.Invalid(msgExpectedBoth + name)
	case len(ev.AttributeValueList) > 0:
		return expression.LegacyTest{}, value.Invalid(msgExpectedListOnly + name)
	case ev.Exists == nil && ev.Value == nil:
		return expression.LegacyTest{}, value.Invalid(msgExistsNull + name)
	}

	if ev.Exists != nil && !*ev.Exists {
		if ev.Value != nil {
			return expression.LegacyTest{}, value.Invalid(msgExistsFalse + name)
		}

		return expression.LegacyTest{Name: name, Op: expression.LegacyNull}, nil
	}

	if ev.Value == nil {
		return expression.LegacyTest{}, value.Invalid(msgExistsTrue + name)
	}

	return legacyTest(name, new(expression.LegacyEQ), []api.AttributeValue{*ev.Value})
}

// legacyExpectedCompare is an Expected entry with a ComparisonOperator, whose
// one value may be given as Value.
func legacyExpectedCompare(name string, ev *api.ExpectedAttributeValue) (expression.LegacyTest, error) {
	op := *ev.ComparisonOperator
	values := ev.AttributeValueList

	switch {
	case ev.Exists != nil:
		return expression.LegacyTest{}, value.Invalid(msgExpectedExists + name)
	case ev.Value != nil && len(values) > 0:
		return expression.LegacyTest{}, value.Invalid(msgExpectedBoth + name)
	case ev.Value != nil:
		values = []api.AttributeValue{*ev.Value}
	default:
	}

	if len(values) == 0 && op != expression.LegacyNull && op != expression.LegacyNotNull {
		return expression.LegacyTest{}, value.Invalid(fmt.Sprintf(msgExpectedNoValue, op, name))
	}

	return legacyTest(name, ev.ComparisonOperator, values)
}

// checkWriteMixed refuses a write's AttributeUpdates (updates of them),
// Expected and ConditionalOperator together with any of its expression
// parameters, naming the legacy ones in that order.
func checkWriteMixed(
	updates int, expected map[string]api.ExpectedAttributeValue, op *string, exprs ...namedExpr,
) error {
	params := make([]param, 0, len(exprs))
	for _, e := range exprs {
		params = append(params, param{e.name, e.src != nil})
	}

	return checkMixedParams([]param{
		{paramAttributeUpdates, updates > 0}, {paramExpected, len(expected) > 0}, {paramConditionalOp, op != nil},
	}, params)
}
