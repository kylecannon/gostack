package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
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
	out, err := p.runStatement(forStatement(ctx), in)
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

// checkStatementText makes the check a batch and a transaction make of each
// statement as it takes it up, before they parse it: it is given and holds at
// most 8,192 bytes, or the whole call is refused. empty is the message for a
// statement missing or empty.
func checkStatementText(src *string, empty string) error {
	switch {
	case src == nil || *src == "":
		return value.Invalid(empty)
	case len(*src) > maxStatementBytes:
		return value.Invalid(msgStatementLong)
	default:
		return nil
	}
}

// mixing refuses the call at the first statement that is a read where the
// first was a write, or the other way, as the image does: before it looks at
// the statements after that one, and so before an empty or long one among them,
// and whatever is wrong with the statement itself but its syntax (SQ-467
// image-probe.log). A statement that did not parse has no kind.
type mixing struct {
	seen  bool
	reads bool
	msg   string
}

// check refuses stmt when it mixes reads and writes with the statements
// before it.
func (m *mixing) check(stmt *partiql.Statement) error {
	if stmt == nil {
		return nil
	}

	reads := stmt.Kind == partiql.Select

	switch {
	case !m.seen:
		m.seen, m.reads = true, reads
	case reads != m.reads:
		return value.Invalid(m.msg)
	default:
	}

	return nil
}

// parseStatement parses a request's statement and checks it has a value for
// each of its parameters; one that does not is returned with the error, as it
// has a kind (mixing). An EXISTS parses only where exists allows it, in a
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
		return nil, parseFailure(err, exists)
	}

	if stmt.Kind == partiql.Exists && !exists {
		return nil, value.Invalid(msgExistsOnlyTx)
	}

	if stmt.Params > len(params) {
		return stmt, value.Invalid(partiql.MsgParamCount)
	}

	return stmt, nil
}

// parseFailure is the answer to a statement that did not parse. Outside a
// transaction (exists false) an EXISTS around an expression is refused as an
// EXISTS, where inside one it is not well formed.
//
//nolint:revive // flag-parameter: whether the operation runs EXISTS
func parseFailure(err error, exists bool) error {
	pe, ok := errors.AsType[*partiql.Error](err)
	if ok && pe.ExistsOnly && !exists {
		return value.Invalid(msgExistsOnlyTx)
	}

	return statementError(err)
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

	// The read shapes each item it keeps by paths below the top level.
	if aliases != nil {
		ctx = withShape(ctx, &selectShape{sel: selectionOf(aliases), paths: aliases})
	}

	pg, err := p.runSelect(ctx, q, stmt.Where, key, r)
	if err != nil {
		return nil, err
	}

	// A page with nothing projected holds the items as stored, which go out
	// as they are.
	return &api.ExecuteStatementOutput{
		Items: pg.items, ItemsJSON: pg.itemsJSON,
		NextToken: encodeToken(pg.last), LastEvaluatedKey: pg.last, ConsumedCapacity: pg.used,
	}, nil
}

// selectShape is how the read of a SELECT of paths below the top level takes
// them from each item: it decodes the parts sel names, and puts each path
// under its alias (shapeSelected).
type selectShape struct {
	sel   *value.Selection
	paths []partiql.Path
}

// selectionOf is the parts of an item a SELECT of paths takes: each path's
// names, up to its first index, below which the value is taken whole.
func selectionOf(paths []partiql.Path) *value.Selection {
	sel := &value.Selection{}
	names := make([]string, 0, len(paths[0]))

	for _, path := range paths {
		names = names[:0]

		for _, s := range path {
			if s.IsIndex {
				break
			}

			names = append(names, s.Name)
		}

		sel.Add(names)
	}

	return sel
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

// selectPage is one page a SELECT read: its items as maps, or, when it
// projects nothing, as stored.
type selectPage struct {
	items     []map[string]api.AttributeValue
	itemsJSON []awsjson.Encoded
	last      value.Item
	used      *api.ConsumedCapacity
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

		return &selectPage{
			items: out.Items, itemsJSON: out.ItemsJSON, last: out.LastEvaluatedKey, used: out.ConsumedCapacity,
		}, nil
	}

	start, ok := startFrom(q, plan, key, r)
	if !ok {
		return &selectPage{items: []map[string]api.AttributeValue{}}, nil
	}

	out, err := p.Query(withUnwritten(ctx, plan.unwritten), &api.QueryInput{
		TableName: q.table, IndexName: q.index, KeyConditionExpression: &plan.keyCond, FilterExpression: plan.filter,
		ProjectionExpression: q.projection, ExpressionAttributeNames: r.attributeNames(),
		ExpressionAttributeValues: r.attributeValues(), Limit: q.limit, ConsistentRead: q.consistent,
		ExclusiveStartKey: start, ScanIndexForward: new(q.forward), ReturnConsumedCapacity: q.capacity,
	})
	if err != nil {
		return nil, fmt.Errorf("statement: %w", err)
	}

	return &selectPage{
		items: out.Items, itemsJSON: out.ItemsJSON, last: out.LastEvaluatedKey, used: out.ConsumedCapacity,
	}, nil
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

	// The key condition holds the hash equality and a range condition, each
	// one operator, and the filter holds neither them nor the ANDs after them.
	unwritten := conditionAndAND

	if rangeCond != nil {
		var s string

		s, err = r.cond(rangeCond)
		if err != nil {
			return nil, err
		}

		keyCond += " AND " + s
		unwritten += conditionAndAND
	}

	filter, err := renderCond(joinAll(rest), r)
	if err != nil {
		return nil, err
	}

	return &queryPlan{keyCond: keyCond, filter: filter, unwritten: unwritten}, nil
}

// conditionAndAND is the operators of a key condition the image counts toward
// its WHERE, and the filter does not hold: the condition itself and the AND
// that follows it (SQ-454).
const conditionAndAND = 2

// queryPlan is the expressions of the Query a SELECT runs as.
type queryPlan struct {
	keyCond string
	filter  *string
	// unwritten is the operators the WHERE wrote for the key condition, which
	// the count of the filter's takes in.
	unwritten int
}

// startFrom is where the Query of plan starts after q.start, a NextToken that
// another statement may have given. DynamoDB Local orders the start key with
// the whole table's keys and reads what follows it that meets the key
// condition (verification/SQ-229 probe6): from a lower hash key the Query
// reads all of its own items, from a higher one none, and a start key on
// either side of the range condition starts at the beginning or reads
// nothing. ok is false for nothing. A start key the key condition cannot
// judge is left to the Query to refuse.
func startFrom(q *selectQuery, plan *queryPlan, key table.KeySchema, r *render) (value.Item, bool) {
	if q.start == nil {
		return nil, true
	}

	kc, err := expression.ParseKeyCondition(plan.keyCond, key,
		expression.Substitutions{Names: r.attributeNames(), Values: r.attributeValues()}, map[string]struct{}{})
	if err != nil {
		return q.start, true
	}

	hashName, hash := kc.Hash()

	sv, has := q.start[hashName]
	if !has {
		return q.start, true
	}

	if !value.Equal(&sv, &hash) {
		return nil, value.Compare(&hash, &sv) > 0
	}

	rangeName, cond, ok := kc.Range()

	rv, has := q.start[rangeName]
	if !ok || !has || cond.Meets(&rv) {
		return q.start, true
	}

	return nil, startBelow(&rv, cond) == q.forward
}

// startBelow reports whether v, which does not meet cond, sorts before the
// range key values that do. Those form one run of the sort order, so v is
// before or after all of them.
func startBelow(v *api.AttributeValue, cond table.SortCondition) bool {
	switch cond.Op {
	case table.SortLT, table.SortLE:
		return false
	case table.SortGT, table.SortGE:
		return true
	default: // SortEQ, SortBetween, SortBeginsWith: the first value is the low end
		return value.Compare(v, &cond.Values[0]) < 0
	}
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
	aliases := make(map[string]struct{}, len(paths))

	var top []string

	nested := false

	for i, path := range paths {
		alias := path.Alias()
		if _, dup := aliases[alias]; dup {
			return nil, nil, value.Invalid(msgDuplicateAlias + alias)
		}

		aliases[alias] = struct{}{}

		// DynamoDB Local adds a SELECT's paths to its overlap trie first to
		// last, which ParseProjection does last to first (SQ-229 probe6,
		// SQ-333), so the check is given them reversed.
		full[len(paths)-1-i] = whole.path(path)
		nested = nested || len(path) > 1

		if t := r.path(path[:1]); !slices.Contains(top, t) {
			top = append(top, t)
		}
	}

	_, err := expression.ParseStatementProjection(strings.Join(full, ", "),
		expression.Substitutions{Names: whole.attributeNames()}, map[string]struct{}{})
	if err != nil {
		return nil, nil, fmt.Errorf("statement: %w", err)
	}

	// The image holds a SELECT's paths to no size, and so does the read this
	// list goes to, which parses it with no size of its own (forStatement).
	// Nested paths add nothing to it: the read keeps the top-level attribute
	// and takes the paths from it (selectShape).
	text := strings.Join(top, ", ")

	if !nested {
		return &text, nil, nil
	}

	return &text, paths, nil
}

// wholePaths renders paths as a ProjectionExpression of the paths themselves.
func wholePaths(paths []partiql.Path, r *render) *string {
	texts := make([]string, len(paths))
	for i, path := range paths {
		texts[i] = r.path(path)
	}

	text := strings.Join(texts, ", ")

	return &text
}

// shapeItem puts each projected path of item under its alias; with no paths,
// the item is as read. A long list is counted against the item (heldPaths) to
// size the result, which suits an item that holds all that its source gave: an
// index's part of a stored item.
func shapeItem(item value.Item, paths []partiql.Path) value.Item {
	if item == nil || paths == nil {
		return item
	}

	size := len(paths)
	if size > shapeHintMax {
		size = heldPaths(item, paths)
	}

	return shapeInto(item, paths, size)
}

// shapeSelected is shapeItem for an item the read decoded with only the parts
// the paths name (Decoder.ViewSelect). What the decoder kept bounds what the
// paths can hold, so a long list sizes the result from it (keptMembers) and
// each path is looked up once, in shapeInto.
func shapeSelected(item value.Item, paths []partiql.Path) value.Item {
	if item == nil || paths == nil {
		return item
	}

	size := len(paths)
	if size > shapeHintMax {
		size = min(size, keptMembers(item))
	}

	return shapeInto(item, paths, size)
}

// keptMembers is how many values the paths of a SELECT can take from item,
// which the read decoded with only the parts they name: each member of a map,
// which the decoder kept for a path, and one for any other value. It is a bound
// and not a count: a value a path takes whole counts its members, which the
// result holds once, and a list counts one for however many elements its paths
// cut it at.
func keptMembers(item value.Item) int {
	n := 0

	for _, v := range item {
		if v.M != nil {
			n += len(v.M)
		} else {
			n++
		}
	}

	return n
}

// shapeInto is the result of a SELECT of paths for item, in a map made for size
// entries.
//
// A short list names mostly what the item holds, so the result is sized for
// it, and a map that grows instead costs a narrow SELECT an allocation and 204
// bytes an item (SQ-452). Most of a long list names nothing the item holds, and
// a map sized for it costs every item the list's length (SQ-412), so the caller
// sizes the map for what the item holds: no more than the list's length, and a
// map that grows from small to that costs 21% more (SQ-456).
func shapeInto(item value.Item, paths []partiql.Path, size int) value.Item {
	out := make(value.Item, size)

	var cuts cutPaths

	for _, path := range paths {
		if i := indexCut(path); i > 0 {
			cuts.add(path, i)

			continue
		}

		if v, ok := lookupPath(item, path); ok {
			out[path.Alias()] = v
		}
	}

	cuts.put(out, item)

	return out
}

// shapeHintMax is the longest list shapeItem sizes its result for without
// counting what the item holds of it.
const shapeHintMax = 32

// heldPaths is how many of paths item holds.
func heldPaths(item value.Item, paths []partiql.Path) int {
	n := 0

	for _, path := range paths {
		if _, ok := lookupPath(item, path); ok {
			n++
		}
	}

	return n
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
