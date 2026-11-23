package dynamodb

import (
	"context"
	"fmt"
	"slices"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
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
// UnprocessedKeys. The legacy AttributesToGet projects as ProjectionExpression does.
func (p *Provider) BatchGetItem(ctx context.Context, in *api.BatchGetItemInput) (*api.BatchGetItemOutput, error) {
	gets, err := p.caller(ctx).checkBatchGet(in.RequestItems)
	if err != nil {
		return nil, err
	}

	out := &api.BatchGetItemOutput{
		UnprocessedKeys: map[string]api.KeysAndAttributes{},
	}
	used := tableConsumption{}
	read, full := 0, false

	// The items go out as they are stored when no table's are projected: the
	// encoder writes Responses or ResponsesJSON, not some tables of each.
	encoded := !slices.ContainsFunc(gets, func(g batchGet) bool { return g.projection != nil })
	if encoded {
		out.ResponsesJSON = make(map[string][]awsjson.Encoded, len(gets))
	} else {
		out.Responses = make(map[string][]map[string]api.AttributeValue, len(gets))
	}

	for _, g := range gets {
		items := make([]value.Packed, 0, len(g.request.Keys))
		cost := used.on(g.name)

		for i, key := range g.request.Keys {
			if full {
				out.UnprocessedKeys[g.name] = g.unprocessed(i)

				break
			}

			stored := g.entry.data.GetPacked(key)
			if stored == "" {
				continue
			}

			size := stored.Size()
			if read+size > maxBatchGetBytes {
				full = true
				out.UnprocessedKeys[g.name] = g.unprocessed(i)

				break
			}

			read += size
			cost.table += readUnits(size, g.request.ConsistentRead)

			items = append(items, stored)
		}

		// A table named once by name and once by ARN answers under its name.
		if encoded {
			out.ResponsesJSON[g.name] = appendEncoded(out.ResponsesJSON[g.name], items)
		} else {
			out.Responses[g.name] = appendTyped(out.Responses[g.name], items, g.projection)
		}
	}

	out.ConsumedCapacity = used.report(in.ReturnConsumedCapacity)

	return out, nil
}

// appendEncoded appends the json of items to list, which it makes when it is
// nil, so a table with no items answers an empty list.
func appendEncoded(list []awsjson.Encoded, items []value.Packed) []awsjson.Encoded {
	if list == nil {
		list = make([]awsjson.Encoded, 0, len(items))
	}

	for _, item := range items {
		list = append(list, item.Encoded())
	}

	return list
}

// appendTyped appends items, decoded and projected by projection when it is
// not nil, to list, which it makes when it is nil.
func appendTyped(list []map[string]api.AttributeValue, items []value.Packed, projection expression.Projection,
) []map[string]api.AttributeValue {
	if list == nil {
		list = make([]map[string]api.AttributeValue, 0, len(items))
	}

	names := distinct(topNames(projection), nil)

	var dec value.Decoder

	for _, item := range items {
		if projection == nil {
			list = append(list, dec.Item(item, nil))
		} else {
			list = append(list, projection.Apply(dec.View(item, names)))
		}
	}

	return list
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

	// DynamoDB Local refuses AttributesToGet before it looks for the table.
	err = checkMixed(request.AttributesToGet, namedExpr{paramProjection, request.ProjectionExpression})
	if err != nil {
		return batchGet{}, err
	}

	legacy, err := attributesToGet(request.AttributesToGet, batchNameTwice)
	if err != nil {
		return batchGet{}, err
	}

	_, e, err := c.lookup(&name)
	if err != nil {
		return batchGet{}, err
	}

	err = checkBatchKeys(e, name, request.Keys)
	if err != nil {
		return batchGet{}, err
	}

	x, err := newExprs(c.from, request.ExpressionAttributeNames, nil, request.ProjectionExpression != nil, false, "")
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

	if projection == nil {
		projection = legacy
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
