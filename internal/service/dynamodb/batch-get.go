package dynamodb

import (
	"context"
	"fmt"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The messages DynamoDB Local answers for a BatchGetItem it refuses.
const (
	msgBatchGetTooMany = "Too many items requested for the BatchGetItem call"
	msgBatchGetNone    = "BatchGetItem must have some requests set"
	msgBatchGetEmpty   = "The list of keys in RequestItems for BatchGetItem is required: %s has empty list"
	msgBatchGetNullKey = "The list of keys in RequestItems for BatchGetItem may not contain null or empty values: " +
		"%s has one or more empty keys"
)

// batchGet is one checked table of a BatchGetItem.
type batchGet struct {
	name       string
	entry      *tableEntry
	request    api.KeysAndAttributes
	projection expression.Projection
}

// BatchGetItem reads up to 100 items across tables, each table's projected by
// its own ProjectionExpression. It stops adding items once the next would take
// the stored bytes read past 16 MB, and returns the keys it did not read in
// UnprocessedKeys. The legacy AttributesToGet answers 501.
func (p *Provider) BatchGetItem(ctx context.Context, in *api.BatchGetItemInput) (*api.BatchGetItemOutput, error) {
	gets, err := p.caller(ctx).checkBatchGet(in.RequestItems)
	if err != nil {
		return nil, err
	}

	out := &api.BatchGetItemOutput{
		Responses:       make(map[string][]map[string]api.AttributeValue, len(gets)),
		UnprocessedKeys: map[string]api.KeysAndAttributes{},
	}
	used := tableConsumption{}
	read, full := 0, false

	for _, g := range gets {
		items := make([]map[string]api.AttributeValue, 0, len(g.request.Keys))
		cost := used.on(g.name)

		for i, key := range g.request.Keys {
			if full {
				out.UnprocessedKeys[g.name] = g.unprocessed(i)

				break
			}

			stored := g.entry.data.Get(key)
			if stored == nil {
				continue
			}

			size := value.ItemSize(stored)
			if read+size > maxBatchGetBytes {
				full = true
				out.UnprocessedKeys[g.name] = g.unprocessed(i)

				break
			}

			read += size
			cost.table += readUnits(size, g.request.ConsistentRead)

			if g.projection != nil {
				stored = g.projection.Apply(stored)
			}

			items = append(items, stored)
		}

		// A table named once by name and once by ARN answers under its name.
		if earlier, ok := out.Responses[g.name]; ok {
			items = append(earlier, items...)
		}

		out.Responses[g.name] = items
	}

	out.ConsumedCapacity = used.report(in.ReturnConsumedCapacity)

	return out, nil
}

// unprocessed is the request for the keys from the i-th on, as the caller
// sends it again.
func (g *batchGet) unprocessed(i int) api.KeysAndAttributes {
	rest := g.request
	rest.Keys = g.request.Keys[i:]

	return rest
}

// checkBatchGet checks a BatchGetItem's tables and keys, as DynamoDB Local
// orders its checks: the count, then table by table in its order.
func (c caller) checkBatchGet(requests map[string]api.KeysAndAttributes) ([]batchGet, error) {
	count := 0
	for _, r := range requests {
		count += len(r.Keys)
	}

	if count > maxBatchGetKeys {
		return nil, value.Invalid(msgBatchGetTooMany)
	}

	if len(requests) == 0 {
		return nil, value.Invalid(msgBatchGetNone)
	}

	names := requestTables(requests)
	gets := make([]batchGet, 0, len(names))

	for _, name := range names {
		g, err := c.checkBatchGetTable(name, requests[name])
		if err != nil {
			return nil, err
		}

		gets = append(gets, g)
	}

	return gets, nil
}

func (c caller) checkBatchGetTable(name string, request api.KeysAndAttributes) (batchGet, error) {
	name = tableName(name)

	err := checkName(&name)
	if err != nil {
		return batchGet{}, err
	}

	if len(request.Keys) == 0 {
		return batchGet{}, value.Invalid(fmt.Sprintf(msgBatchGetEmpty, name))
	}

	_, e, err := c.lookup(&name)
	if err != nil {
		return batchGet{}, err
	}

	err = checkBatchKeys(e, name, request.Keys)
	if err != nil {
		return batchGet{}, err
	}

	if request.AttributesToGet != nil {
		return batchGet{}, notImplemented("BatchGetItem")
	}

	x, err := newExprs(request.ExpressionAttributeNames, nil, request.ProjectionExpression != nil, false, "")
	if err != nil {
		return batchGet{}, err
	}

	projection, err := x.projection(request.ProjectionExpression)
	if err == nil {
		err = x.done()
	}

	if err != nil {
		return batchGet{}, fmt.Errorf("batch get: %w", err)
	}

	return batchGet{name: name, entry: e, request: request, projection: projection}, nil
}

// checkBatchKeys checks one table's keys: none empty, none twice, and then
// each against the table's key schema.
func checkBatchKeys(e *tableEntry, name string, keys []map[string]api.AttributeValue) error {
	for _, key := range keys {
		if len(key) == 0 {
			return value.Invalid(fmt.Sprintf(msgBatchGetNullKey, name))
		}

		err := value.ValidateItem(key)
		if err != nil {
			return fmt.Errorf("batch get key: %w", err)
		}
	}

	// The keys are not yet known to fit the schema, so they are compared
	// whole: at most 100 of them.
	for i, key := range keys {
		for _, earlier := range keys[:i] {
			if value.EqualItems(key, earlier) {
				return value.Invalid(msgBatchDuplicates)
			}
		}
	}

	for _, key := range keys {
		err := e.data.CheckKey(key)
		if err != nil {
			return fmt.Errorf("batch get key: %w", err)
		}
	}

	return nil
}
