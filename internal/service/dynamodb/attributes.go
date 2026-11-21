package dynamodb

import (
	"fmt"
	"slices"
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
	// msgIndexFilterNotProjected is a Query's, which the image words with
	// "Secondary index" alone.
	msgIndexFilterNotProjected = "One or more parameter values were invalid: " +
		"Secondary index %s does not project one or more filter attributes: [%s]"
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

// attributesToGet sets the read's projection to AttributesToGet's; a global
// index must hold them (checkIndexProjects).
func (r *reader) attributesToGet(names []string) error {
	projection, err := attributesToGet(names, sameNameTwice)
	if err != nil {
		return err
	}

	r.projection = projection

	return nil
}

// checkIndexProjects refuses the top-level names of an AttributesToGet, a
// ProjectionExpression or a SELECT list that a global index read does not
// hold, listed in Java's HashSet order as DynamoDB Local lists them. A global
// index holds the attributes it projects: AWS's Query and Scan pages say "If
// you query or scan a global secondary index, you can only request attributes
// that are projected into the index. Global secondary index queries cannot
// fetch attributes from the parent table.".
func (r *reader) checkIndexProjects(names []string) error {
	missing := r.unheld(names)
	if len(missing) == 0 {
		return nil
	}

	ordered := strings.Join(expression.JavaHashOrder(missing), ", ")

	return value.Invalid(fmt.Sprintf(msgIndexNotProjected, r.index.Name, ordered))
}

// checkProjection makes the checks a read's projection meets on an index: a
// global index must hold what it names (checkIndexProjects), and a local index
// that does not decides to fetch from the table (decideFetch). The names are
// listed once.
func (r *reader) checkProjection() error {
	names := topNames(r.projection)

	err := r.checkIndexProjects(names)
	if err != nil {
		return err
	}

	r.decideFetch(names)

	return nil
}

// checkFilterProjected refuses a Query's filter, whose attribute names are
// given, that names one a global index does not hold. A statement's WHERE is
// left alone, as the image leaves it (docs/compat.md).
func (r *reader) checkFilterProjected(names []string) error {
	if r.from == fromStatement {
		return nil
	}

	missing := r.unheld(names)
	if len(missing) == 0 {
		return nil
	}

	// The image lists these in name order, where it lists a projection's in
	// Java's HashSet order.
	slices.Sort(missing)

	return value.Invalid(fmt.Sprintf(msgIndexFilterNotProjected, r.index.Name, strings.Join(missing, ", ")))
}

// unheld lists the names, each once, that a read of a global index that does
// not project every attribute does not hold. It is nil for a read of the table
// or of a local index, which fetches from the table.
func (r *reader) unheld(names []string) []string {
	if r.index == nil || !r.index.Global || r.index.Projection.Type == "" || r.index.Projection.Type == table.ProjectAll {
		return nil
	}

	var missing []string

	for _, name := range names {
		if !r.holds(name) && !slices.Contains(missing, name) {
			missing = append(missing, name)
		}
	}

	return missing
}
