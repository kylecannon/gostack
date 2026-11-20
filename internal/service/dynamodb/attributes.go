package dynamodb

import (
	"fmt"
	"strings"

	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/kylecannon/gostack/internal/service/dynamodb/table"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The messages DynamoDB Local answers for a legacy AttributesToGet it
// refuses, as the LocalStack image returns them (verification/SQ-302 q1, q4).
const (
	msgMixedParams = "Can not use both expression and non-expression parameters in the same request: " +
		"Non-expression parameters: {%s} Expression parameters: {%s}"
	msgAttrEmpty         = "Empty attribute name"
	msgAttrTwice         = "Cannot have two attributes with the same name"
	msgBatchAttrTwice    = "One or more parameter values were invalid: Duplicate value in attribute name: "
	msgIndexNotProjected = "One or more parameter values were invalid: Global secondary index %s does not project [%s]"
)

// The request parameters the AttributesToGet messages name.
const (
	paramAttributesToGet = "AttributesToGet"
	paramProjection      = "ProjectionExpression"
	paramFilter          = "FilterExpression"
	paramKeyCondition    = "KeyConditionExpression"
)

// namedExpr is one expression parameter of a request and its text, nil when
// it is not set.
type namedExpr struct {
	name string
	src  *string
}

// param is a request parameter and whether the request sets it.
type param struct {
	name string
	set  bool
}

// checkMixed refuses AttributesToGet in a request that sets any of exprs, which
// are listed in DynamoDB Local's order. An empty AttributesToGet is not set.
func checkMixed(names []string, exprs ...namedExpr) error {
	params := make([]param, 0, len(exprs))
	for _, e := range exprs {
		params = append(params, param{e.name, e.src != nil})
	}

	return checkMixedParams([]param{{paramAttributesToGet, len(names) > 0}}, params)
}

// checkMixedParams refuses a request that sets a legacy, non-expression
// parameter and an expression parameter. Each list is in the order DynamoDB
// Local names its parameters.
func checkMixedParams(legacy, exprs []param) error {
	nonExpr, expr := setNames(legacy), setNames(exprs)
	if len(nonExpr) == 0 || len(expr) == 0 {
		return nil
	}

	return value.Invalid(fmt.Sprintf(msgMixedParams, strings.Join(nonExpr, ", "), strings.Join(expr, ", ")))
}

// setNames are the names of the params the request sets.
func setNames(params []param) []string {
	var names []string

	for _, p := range params {
		if p.set {
			names = append(names, p.name)
		}
	}

	return names
}

// attributesToGet checks the legacy AttributesToGet's names in turn, none
// empty and none twice, and returns them as the projection they ask for, or
// nil when there are none, which projects nothing away. twice is the message
// for a name given twice.
//
//nolint:ireturn // the projection is expression's interface
func attributesToGet(names []string, twice func(name string) string) (expression.Projection, error) {
	if len(names) == 0 {
		return nil, nil //nolint:nilnil // no projection
	}

	seen := make(map[string]struct{}, len(names))

	for _, name := range names {
		if name == "" {
			return nil, value.Invalid(msgAttrEmpty)
		}

		if _, dup := seen[name]; dup {
			return nil, value.Invalid(twice(name))
		}

		seen[name] = struct{}{}
	}

	return expression.ProjectAttributes(names), nil
}

// sameNameTwice is GetItem's and Scan's message for a name given twice.
func sameNameTwice(string) string {
	return msgAttrTwice
}

// batchNameTwice is BatchGetItem's.
func batchNameTwice(name string) string {
	return msgBatchAttrTwice + name
}

// projectionParam names the projection parameter a Query or Scan sets, for
// its Select messages, or "" when it sets none.
func projectionParam(src *string, names []string) string {
	switch {
	case src != nil:
		return paramProjection
	case len(names) > 0:
		return paramAttributesToGet
	default:
		return ""
	}
}

// attributesToGet sets the read's projection to AttributesToGet's, which a
// global index must hold.
func (r *reader) attributesToGet(names []string) error {
	projection, err := attributesToGet(names, sameNameTwice)
	if err != nil {
		return err
	}

	err = r.checkIndexProjects(names)
	if err != nil {
		return err
	}

	r.projection = projection

	return nil
}

// checkIndexProjects refuses AttributesToGet names a global index read does
// not hold, listed in Java's HashSet order as DynamoDB Local lists them.
func (r *reader) checkIndexProjects(names []string) error {
	if r.index == nil || !r.index.Global || r.index.Projection.Type == "" || r.index.Projection.Type == table.ProjectAll {
		return nil
	}

	var missing []string

	for _, name := range names {
		if !r.holds(name) {
			missing = append(missing, name)
		}
	}

	if len(missing) == 0 {
		return nil
	}

	ordered := strings.Join(expression.JavaHashOrder(missing), ", ")

	return value.Invalid(fmt.Sprintf(msgIndexNotProjected, r.index.Name, ordered))
}
