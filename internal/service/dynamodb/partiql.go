package dynamodb

import (
	"context"
	"fmt"
	"slices"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/protocol/constraint"
	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/kylecannon/gostack/internal/service/dynamodb/partiql"
	"github.com/kylecannon/gostack/internal/service/dynamodb/table"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// codeConditionFailed is ConditionalCheckFailedException's code.
const codeConditionFailed = "ConditionalCheckFailedException"

// ExecuteStatement runs one PartiQL statement. A statement compiles to the
// expressions and the operation the equivalent call would make: a SELECT
// whose WHERE pins the hash key is a Query and any other a Scan; INSERT,
// UPDATE and DELETE are PutItem, UpdateItem and DeleteItem on the one item
// their key names.
func (p *Provider) ExecuteStatement(
	ctx context.Context, in *api.ExecuteStatementInput,
) (*api.ExecuteStatementOutput, error) {
	out, err := p.runStatement(ctx, in)
	if err != nil {
		return nil, statementFailure(err)
	}

	return out, nil
}

func (p *Provider) runStatement(
	ctx context.Context, in *api.ExecuteStatementInput,
) (*api.ExecuteStatementOutput, error) {
	err := in.Validate()
	if err != nil {
		return nil, err
	}

	if in.Limit != nil && *in.Limit < 1 {
		var r constraint.Report
		r.Value(nil, "limit", constraint.Number(*in.Limit), "Member must have value greater than or equal to 1")

		return nil, value.Invalid(r.Message())
	}

	returnOld, err := returnOnConditionFailure(in.ReturnValuesOnConditionCheckFailure)
	if err != nil {
		return nil, err
	}

	stmt, err := parseStatement(in.Statement, in.Parameters, false)
	if err != nil {
		return nil, err
	}

	if stmt.Kind == partiql.Select {
		return p.executeSelect(ctx, in, stmt)
	}

	w, err := p.caller(ctx).compileWrite(stmt, in.Parameters, returnOld)
	if err != nil {
		return nil, err
	}

	item, used, err := p.runWrite(ctx, w, in.ReturnConsumedCapacity)
	if err != nil {
		return nil, err
	}

	out := &api.ExecuteStatementOutput{Items: []map[string]api.AttributeValue{}, ConsumedCapacity: used}
	if item != nil {
		out.Items = append(out.Items, item)
	}

	return out, nil
}

// parseStatement parses a request's statement and checks it has a value for
// each of its parameters. An EXISTS parses only where exists allows it, in a
// transaction.
//
//nolint:revive // flag-parameter: whether the operation runs EXISTS
func parseStatement(src *string, params []api.AttributeValue, exists bool) (*partiql.Statement, error) {
	if src == nil {
		return nil, value.Invalid(msgStatementEmpty)
	}

	if len(*src) > maxStatementBytes {
		return nil, value.Invalid(msgStatementLong)
	}

	stmt, err := partiql.Parse(*src)
	if err != nil {
		return nil, statementError(err)
	}

	if stmt.Kind == partiql.Exists && !exists {
		return nil, value.Invalid(partiql.MsgNotWellFormed + *src)
	}

	if stmt.Params > len(params) {
		return nil, value.Invalid(partiql.MsgParamCount)
	}

	return stmt, nil
}

// executeSelect runs a SELECT as a Query or a Scan.
func (p *Provider) executeSelect(
	ctx context.Context, in *api.ExecuteStatementInput, stmt *partiql.Statement,
) (*api.ExecuteStatementOutput, error) {
	_, e, err := p.caller(ctx).statementTable(&stmt.Table)
	if err != nil {
		return nil, fmt.Errorf("statement: %w", err)
	}

	key := e.data.Key()

	var index *string

	if stmt.Index != "" {
		spec, ok := e.findIndex(stmt.Index)
		if !ok {
			return nil, value.Invalid(msgScanNoIndex + stmt.Index)
		}

		key, index = spec.Key, &stmt.Index
	}

	if stmt.OrderBy != nil && stmt.Where == nil {
		return nil, value.Invalid(msgOrderNoWhere)
	}

	start, err := decodeToken(in.NextToken)
	if err != nil {
		return nil, err
	}

	r := newRender(in.Parameters)

	projected, aliases, err := projection(stmt.Projection, r)
	if err != nil {
		return nil, err
	}

	q := &selectQuery{
		table: &stmt.Table, index: index, projection: projected, limit: in.Limit, consistent: in.ConsistentRead,
		start: start, forward: !stmt.Desc, capacity: in.ReturnConsumedCapacity,
	}

	pg, err := p.runSelect(ctx, q, stmt.Where, key, r)
	if err != nil {
		return nil, err
	}

	return &api.ExecuteStatementOutput{
		Items: shapeItems(pg.items, aliases), NextToken: encodeToken(pg.last), LastEvaluatedKey: pg.last,
		ConsumedCapacity: pg.used,
	}, nil
}

// selectQuery is what a SELECT asks of the Query or Scan it runs as.
type selectQuery struct {
	table, index, projection *string
	limit                    *int32
	consistent               *bool
	start                    value.Item
	forward                  bool
	capacity                 *string
}

// selectPage is one page a SELECT read.
type selectPage struct {
	items []map[string]api.AttributeValue
	last  value.Item
	used  *api.ConsumedCapacity
}

// runSelect runs a SELECT's WHERE over key: a Query when it pins the hash
// key and what is left names no key attribute, which a Query cannot filter
// on, and a Scan otherwise.
func (p *Provider) runSelect(
	ctx context.Context, q *selectQuery, where *partiql.Node, key table.KeySchema, r *render,
) (*selectPage, error) {
	plan, err := planQuery(where, key, r)
	if err != nil {
		return nil, err
	}

	if plan == nil {
		var filter *string

		filter, err = renderCond(where, r)
		if err != nil {
			return nil, err
		}

		var out *api.ScanOutput

		out, err = p.Scan(ctx, &api.ScanInput{
			TableName: q.table, IndexName: q.index, FilterExpression: filter, ProjectionExpression: q.projection,
			ExpressionAttributeNames: r.attributeNames(), ExpressionAttributeValues: r.attributeValues(),
			Limit: q.limit, ConsistentRead: q.consistent, ExclusiveStartKey: q.start, ReturnConsumedCapacity: q.capacity,
		})
		if err != nil {
			return nil, fmt.Errorf("statement: %w", err)
		}

		return &selectPage{items: out.Items, last: out.LastEvaluatedKey, used: out.ConsumedCapacity}, nil
	}

	out, err := p.Query(ctx, &api.QueryInput{
		TableName: q.table, IndexName: q.index, KeyConditionExpression: &plan.keyCond, FilterExpression: plan.filter,
		ProjectionExpression: q.projection, ExpressionAttributeNames: r.attributeNames(),
		ExpressionAttributeValues: r.attributeValues(), Limit: q.limit, ConsistentRead: q.consistent,
		ExclusiveStartKey: q.start, ScanIndexForward: new(q.forward), ReturnConsumedCapacity: q.capacity,
	})
	if err != nil {
		return nil, fmt.Errorf("statement: %w", err)
	}

	return &selectPage{items: out.Items, last: out.LastEvaluatedKey, used: out.ConsumedCapacity}, nil
}

// planQuery finds the key condition of a Query in where: an equality on the
// hash key and, when there is one, the first condition on the range key a key
// condition can hold. ok is false when where pins no hash key, or when what
// is left names a key attribute; the plan is then nil, for a Scan.
func planQuery(where *partiql.Node, key table.KeySchema, r *render) (*queryPlan, error) {
	conds := conjuncts(where)

	hash := slices.IndexFunc(conds, func(c *partiql.Node) bool {
		_, ok := equality(c, key.Hash.Name)

		return ok
	})
	if hash < 0 {
		return nil, nil //nolint:nilnil // no plan: the SELECT is a Scan
	}

	hashValue, _ := equality(conds[hash], key.Hash.Name)
	rest := slices.Delete(slices.Clone(conds), hash, hash+1)

	var rangeCond *partiql.Node

	if key.HasRange() {
		if i := slices.IndexFunc(rest, func(c *partiql.Node) bool { return isRangeCond(c, key.Range.Name) }); i >= 0 {
			rangeCond = rest[i]
			rest = slices.Delete(rest, i, i+1)
		}

		err := checkRangeTypes(rangeCond, rest, key.Range, r)
		if err != nil {
			return nil, err
		}
	}

	if left := joinAll(rest); left != nil && mentions(left, key.Hash.Name, key.Range.Name) {
		return nil, nil //nolint:nilnil // as above
	}

	v, err := r.operand(hashValue)
	if err != nil {
		return nil, err
	}

	keyCond := r.path(partiql.Path{{Name: key.Hash.Name}}) + " = " + v

	if rangeCond != nil {
		var s string

		s, err = r.cond(rangeCond)
		if err != nil {
			return nil, err
		}

		keyCond += " AND " + s
	}

	filter, err := renderCond(joinAll(rest), r)
	if err != nil {
		return nil, err
	}

	return &queryPlan{keyCond: keyCond, filter: filter}, nil
}

// queryPlan is the expressions of the Query a SELECT runs as.
type queryPlan struct {
	keyCond string
	filter  *string
}

// isRangeCond reports whether c is a condition on the range attribute name a
// key condition can hold: a comparison, BETWEEN or begins_with with the
// attribute first and values after it.
func isRangeCond(c *partiql.Node, name string) bool {
	if len(c.Args) < 2 || !isAttr(c.Args[0], name) {
		return false
	}

	for _, a := range c.Args[1:] {
		if !partiql.IsValue(a) {
			return false
		}
	}

	switch c.Kind {
	case partiql.KindCompare:
		return c.Op != "<>"
	case partiql.KindBetween:
		return true
	case partiql.KindCall:
		return c.Name == "begins_with"
	default:
		return false
	}
}

// renderCond renders n as a condition, nil for none.
func renderCond(n *partiql.Node, r *render) (*string, error) {
	if n == nil {
		return nil, nil //nolint:nilnil // no condition
	}

	s, err := r.cond(n)
	if err != nil {
		return nil, err
	}

	return &s, nil
}

// projection renders a SELECT list as a ProjectionExpression of the
// top-level attributes it reads, after checking the whole paths as one. The
// paths come back when one goes below the top level: the result then takes
// each value under its alias.
func projection(paths []partiql.Path, r *render) (*string, []partiql.Path, error) {
	if paths == nil {
		return nil, nil, nil
	}

	whole := newRender(nil)
	full := make([]string, len(paths))

	var top []string

	nested := false

	for i, path := range paths {
		full[i] = whole.path(path)
		nested = nested || len(path) > 1

		if t := r.path(path[:1]); !slices.Contains(top, t) {
			top = append(top, t)
		}
	}

	_, err := expression.ParseProjection(strings.Join(full, ", "),
		expression.Substitutions{Names: whole.attributeNames()}, map[string]struct{}{})
	if err != nil {
		return nil, nil, fmt.Errorf("statement: %w", err)
	}

	text := strings.Join(top, ", ")
	if !nested {
		return &text, nil, nil
	}

	return &text, paths, nil
}

// shapeItems puts each projected path of items under its alias; with no
// paths, items are as read. It never returns nil, so an empty result is [].
func shapeItems(items []map[string]api.AttributeValue, paths []partiql.Path) []map[string]api.AttributeValue {
	out := make([]map[string]api.AttributeValue, len(items))
	for i, item := range items {
		out[i] = shapeItem(item, paths)
	}

	return out
}

// shapeItem puts each projected path of item under its alias; with no paths,
// the item is as read.
func shapeItem(item value.Item, paths []partiql.Path) value.Item {
	if item == nil || paths == nil {
		return item
	}

	out := make(value.Item, len(paths))

	for _, path := range paths {
		if v, ok := lookupPath(item, path); ok {
			out[path.Alias()] = v
		}
	}

	return out
}

// lookupPath is the value at path in item.
func lookupPath(item value.Item, path partiql.Path) (api.AttributeValue, bool) {
	v, ok := item[path[0].Name]

	for _, s := range path[1:] {
		if !ok {
			break
		}

		switch {
		case s.IsIndex && s.Index < len(v.L):
			v = v.L[s.Index]
		case !s.IsIndex && v.M != nil:
			v, ok = v.M[s.Name]
		default:
			ok = false
		}
	}

	return v, ok
}
