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
// order, a page at a time. The legacy KeyConditions, QueryFilter,
// AttributesToGet and ConditionalOperator answer 501.
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
	name, e, err := p.caller(ctx).lookup(in.TableName)
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

	if in.KeyConditions != nil || in.QueryFilter != nil || in.AttributesToGet != nil || in.ConditionalOperator != nil {
		return nil, nil, notImplemented("Query")
	}

	r, err := newReader(e, spec, in.Limit, in.Select, in.ProjectionExpression != nil, in.ConsistentRead)
	if err != nil {
		return nil, nil, err
	}

	r.name = name

	if in.KeyConditionExpression == nil {
		return nil, nil, value.Invalid(msgNoKeyCondition)
	}

	return e, r, nil
}

// newReader makes the checks Query and Scan share before their expressions:
// Limit, Select, then ConsistentRead on a global index.
func newReader(
	e *tableEntry, spec *table.IndexSpec, limit *int32, selectMode *string, hasProjection bool, consistent *bool,
) (*reader, error) {
	n, err := limitOf(limit)
	if err != nil {
		return nil, err
	}

	mode, err := chooseSelect(selectMode, hasProjection, spec != nil)
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
// their placeholders, and refuses a filter on a key attribute.
//
//nolint:ireturn // the parsed form is the expression package's interface
func (r *reader) queryExpressions(in *api.QueryInput) (expression.KeyCondition, error) {
	x, err := newExprs(in.ExpressionAttributeNames, in.ExpressionAttributeValues, true, true, "")
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

	key := r.key

	kc, err := x.keyCondition(*in.KeyConditionExpression, key)
	if err == nil {
		err = x.done()
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

// indexName is the engine's name for the source read: "" for the table.
func indexName(spec *table.IndexSpec) string {
	if spec == nil {
		return ""
	}

	return spec.Name
}
