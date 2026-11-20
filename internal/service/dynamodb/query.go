package dynamodb

import (
	"context"
	"errors"
	"fmt"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/kylecannon/gostack/internal/service/dynamodb/table"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// codeNotFound is the code of ResourceNotFoundException.
const codeNotFound = "ResourceNotFoundException"

// The messages a Query is refused with. msgIndexSelect is LocalStack's own
// (provider.py's query), which names the index id-index whatever it is called;
// no snapshot pins that, so gostack names the index the request asked for.
const (
	msgNoKeyCondition = "Either the KeyConditions or KeyConditionExpression parameter must be specified in the request."
	msgQueryNoIndex   = "Index not found"
	msgUnknownTable   = "Unknown table: "
	msgIndexSelect    = "One or more parameter values were invalid: Select type ALL_ATTRIBUTES " +
		"is not supported for global secondary index "
	msgIndexSelectTail = " because its projection type is not ALL"
	msgFilterOnKey     = "Filter Expression can only contain non-primary key attributes: Primary key attribute: "
)

// Query reads the items of one partition of a table or index, in range key
// order, a page at a time, by its KeyConditionExpression or by the legacy
// KeyConditions, QueryFilter and ConditionalOperator, which translate to the
// same key condition and filter. AttributesToGet goes only with the legacy
// parameters.
func (p *Provider) Query(ctx context.Context, in *api.QueryInput) (*api.QueryOutput, error) {
	e, r, err := p.queryTarget(ctx, in)
	if err != nil {
		return nil, err
	}

	kc, err := r.queryExpressions(in)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}

	hashName, hash := kc.Hash()

	rangeName, cond, ok := kc.Range()
	if !ok {
		cond = table.SortCondition{Op: table.SortAny}
	}

	err = r.checkStart(in.ExclusiveStartKey)
	if err == nil && r.start != nil && !startMeets(r.start, hashName, &hash, rangeName, cond) {
		err = value.Invalid(msgStartQuery)
	}

	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}

	forward := in.ScanIndexForward == nil || *in.ScanIndexForward

	pg, err := r.read(func(visit func(value.Item) bool) error {
		return e.data.Range(indexName(r.index), &hash, cond, r.start, forward, visit)
	})
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}

	used := r.consumed(pg)
	out := pg.output(r.selectMode)

	return &api.QueryOutput{
		Items: out.Items, Count: out.Count, ScannedCount: out.ScannedCount, LastEvaluatedKey: out.LastEvaluatedKey,
		ConsumedCapacity: used.report(in.ReturnConsumedCapacity, r.name),
	}, nil
}

// queryTarget finds the table and index a Query reads and makes the checks
// that come before its expressions, in DynamoDB Local's order, with
// LocalStack's index checks first.
func (p *Provider) queryTarget(ctx context.Context, in *api.QueryInput) (*tableEntry, *reader, error) {
	name, e, err := p.lookupHome(p.caller(ctx), in.TableName)
	if err != nil {
		var notFound *awserr.Error
		if in.IndexName != nil && errors.As(err, &notFound) && notFound.Code == codeNotFound {
			return nil, nil, api.NewResourceNotFoundException(msgUnknownTable + *in.TableName)
		}

		return nil, nil, fmt.Errorf("query: %w", err)
	}

	var spec *table.IndexSpec

	if in.IndexName != nil {
		var ok bool
		if spec, ok = e.findIndex(*in.IndexName); !ok {
			return nil, nil, api.NewResourceNotFoundException(msgQueryNoIndex)
		}

		if spec.Global && in.Select != nil && *in.Select == selectAll && spec.Projection.Type != table.ProjectAll {
			return nil, nil, value.Invalid(msgIndexSelect + *in.IndexName + msgIndexSelectTail)
		}
	}

	err = checkMixedParams([]param{
		{paramAttributesToGet, len(in.AttributesToGet) > 0},
		{paramQueryFilter, len(in.QueryFilter) > 0},
		{paramConditionalOp, in.ConditionalOperator != nil},
		// KeyConditions go with a FilterExpression or ProjectionExpression
		// (probed); only the KeyConditionExpression refuses them.
		{paramKeyConditions, in.KeyConditions != nil && in.KeyConditionExpression != nil},
	}, []param{
		{paramProjection, in.ProjectionExpression != nil},
		{paramFilter, in.FilterExpression != nil},
		{paramKeyCondition, in.KeyConditionExpression != nil},
	})
	if err != nil {
		return nil, nil, err
	}

	legacy, err := queryLegacy(in, e, spec)
	if err != nil {
		return nil, nil, err
	}

	r, err := newReader(e, spec, in.Limit, in.Select, projectionParam(in.ProjectionExpression, in.AttributesToGet),
		in.ConsistentRead)
	if err != nil {
		return nil, nil, err
	}

	r.name, r.legacy = name, legacy

	if in.KeyConditionExpression == nil && legacy == nil {
		return nil, nil, value.Invalid(msgNoKeyCondition)
	}

	return e, r, nil
}

// queryLegacy checks a Query's legacy KeyConditions, QueryFilter and
// ConditionalOperator, which DynamoDB Local does after Limit and before
// Select, and returns them translated; nil when the Query has no
// KeyConditions, which then needs a KeyConditionExpression.
func queryLegacy(in *api.QueryInput, e *tableEntry, spec *table.IndexSpec) (*legacyKey, error) {
	if in.KeyConditions == nil {
		return nil, nil //nolint:nilnil // not a legacy query
	}

	_, err := limitOf(in.Limit)
	if err != nil {
		return nil, err
	}

	schema := e.data.Key()
	if spec != nil {
		schema = spec.Key
	}

	legacy, err := legacyQuery(in, schema)
	if err != nil {
		return nil, err
	}

	return legacy, nil
}

// newReader makes the checks Query and Scan share before their expressions:
// Limit, Select, then ConsistentRead on a global index.
func newReader(
	e *tableEntry, spec *table.IndexSpec, limit *int32, selectMode *string, projection string, consistent *bool,
) (*reader, error) {
	n, err := limitOf(limit)
	if err != nil {
		return nil, err
	}

	mode, err := chooseSelect(selectMode, projection, spec != nil)
	if err != nil {
		return nil, err
	}

	r := &reader{tableKey: e.data.Key(), key: e.data.Key(), selectMode: mode, limit: n, consistent: consistent}
	if spec != nil {
		if spec.Global && consistent != nil && *consistent {
			return nil, value.Invalid(msgConsistentGSI)
		}

		r.index, r.key = spec, spec.Key
	}

	return r, nil
}

// queryExpressions parses a Query's expressions in DynamoDB Local's order,
// FilterExpression, ProjectionExpression, then KeyConditionExpression, checks
// their placeholders, and refuses a filter on a key attribute. A legacy Query
// has its key condition and QueryFilter already; it may still give a
// FilterExpression and a ProjectionExpression, and its AttributesToGet are
// checked here.
//
//nolint:ireturn // the parsed form is the expression package's interface
func (r *reader) queryExpressions(in *api.QueryInput) (expression.KeyCondition, error) {
	hasExpr, valuesUsable := true, true
	if r.legacy != nil {
		hasExpr = in.FilterExpression != nil || in.ProjectionExpression != nil
		valuesUsable = in.FilterExpression != nil
	}

	x, err := newExprs(in.ExpressionAttributeNames, in.ExpressionAttributeValues, hasExpr, valuesUsable, msgNullExprs)
	if err != nil {
		return nil, err
	}

	r.filter, err = x.condition(expression.FilterExpression, in.FilterExpression)
	if err != nil {
		return nil, err
	}

	r.projection, err = x.projection(in.ProjectionExpression)
	if err != nil {
		return nil, err
	}

	var kc expression.KeyCondition

	key := r.key

	if r.legacy != nil {
		kc = r.legacy.key
		if r.filter == nil {
			r.filter = r.legacy.filter
		}

		if len(in.AttributesToGet) > 0 {
			err = r.attributesToGet(in.AttributesToGet)
		}
	} else {
		kc, err = x.keyCondition(*in.KeyConditionExpression, key)
	}

	if err == nil {
		err = x.done()
	}

	if err == nil {
		err = r.checkKeyConditionEmpty(kc)
	}

	if err != nil {
		return nil, err
	}

	for _, name := range expression.Names(r.filter) {
		if name == key.Hash.Name || name == key.Range.Name {
			return nil, value.Invalid(msgFilterOnKey + name)
		}
	}

	r.decideFetch(topNames(r.projection))

	return kc, nil
}

// checkKeyConditionEmpty refuses a key condition that compares a key attribute
// with an empty string or binary, the partition value first, whatever the
// operator: DynamoDB does not take the empty value in a key, so no item can
// match it. It runs after the placeholders are checked and before the filter
// is held to the non-key attributes.
func (r *reader) checkKeyConditionEmpty(kc expression.KeyCondition) error {
	hashName, hash := kc.Hash()

	msg := table.EmptyKeyMessage(r.index, hashName, &hash)
	if msg != "" {
		return value.Invalid(msg)
	}

	rangeName, cond, ok := kc.Range()
	if !ok {
		return nil
	}

	for i := range cond.Values {
		msg = table.EmptyKeyMessage(r.index, rangeName, &cond.Values[i])
		if msg != "" {
			return value.Invalid(msg)
		}
	}

	return nil
}

// indexName is the engine's name for the source read: "" for the table.
func indexName(spec *table.IndexSpec) string {
	if spec == nil {
		return ""
	}

	return spec.Name
}
