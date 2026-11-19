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

// The messages DynamoDB Local answers for a TransactGetItems it refuses.
const (
	msgTransactGetMember = "Value null at 'transactItems.%d.member.get.tableName' failed to satisfy constraint: " +
		"Member must not be null"
	msgTransactGetNoKey = "Get Condition for key not in key list"
)

// txGet is one checked Get of a TransactGetItems.
type txGet struct {
	name       string
	entry      *tableEntry
	key        value.Item
	projection expression.Projection
}

// TransactGetItems reads up to 100 items across tables from one state of
// them: every table's read lock is held across the reads.
func (p *Provider) TransactGetItems(
	ctx context.Context, in *api.TransactGetItemsInput,
) (*api.TransactGetItemsOutput, error) {
	gets, err := p.caller(ctx).checkTransactGet(in.TransactItems)
	if err != nil {
		return nil, err
	}

	stored := readTransaction(gets)
	out := &api.TransactGetItemsOutput{Responses: make([]api.ItemResponse, len(gets))}
	used := tableConsumption{}

	for i, g := range gets {
		// A transactional read costs twice a consistent one, an item or not.
		used.on(g.name).table += transactUnits * readUnits(value.ItemSize(stored[i]), new(true))

		item := stored[i]
		if g.projection != nil && item != nil {
			item = g.projection.Apply(item)
		}

		out.Responses[i].Item = item
	}

	out.ConsumedCapacity = used.report(in.ReturnConsumedCapacity)

	return out, nil
}

// readTransaction reads every get's item under the read locks of all their
// tables, taken in table.Lock's global order.
func readTransaction(gets []txGet) []value.Item {
	engines := make([]*table.Table, len(gets))
	for i := range gets {
		engines[i] = gets[i].entry.data
	}

	unlock := table.ReadLock(engines...)
	defer unlock()

	stored := make([]value.Item, len(gets))
	for i := range gets {
		stored[i] = gets[i].entry.data.GetHeld(gets[i].key)
	}

	return stored
}

// checkTransactGet checks a TransactGetItems' gets in turn, as DynamoDB Local
// does, and refuses a second get of an item.
func (c caller) checkTransactGet(items []api.TransactGetItem) ([]txGet, error) {
	switch {
	case len(items) == 0:
		return nil, value.Invalid(msgTransactNull)
	case len(items) > maxTransactItems:
		return nil, value.Invalid(msgTransactTooMany)
	}

	gets := make([]txGet, len(items))
	tables := map[string]*tableEntry{}
	seen := make(map[*tableEntry]map[string]struct{})

	for i := range items {
		if items[i].Get == nil {
			return nil, value.Invalid(msgTransactNull)
		}

		g, err := c.checkGet(i+1, items[i].Get, tables)
		if err != nil {
			return nil, err
		}

		if seen[g.entry] == nil {
			seen[g.entry] = map[string]struct{}{}
		}

		id := itemKey(g.entry, g.key)
		if _, dup := seen[g.entry][id]; dup {
			return nil, value.Invalid(msgTransactSameItem)
		}

		seen[g.entry][id] = struct{}{}
		gets[i] = g
	}

	return gets, nil
}

// checkGet checks the get at position (from 1). DynamoDB Local answers a key
// the table refuses as a TransactionCanceledException with no reasons.
func (c caller) checkGet(position int, get *api.Get, tables map[string]*tableEntry) (txGet, error) {
	if get.TableName == nil {
		return txGet{}, value.Invalid(fmt.Sprintf(msgTransactGetMember, position))
	}

	name, e, err := c.transactTable(*get.TableName, tables)
	if err != nil {
		return txGet{}, err
	}

	if get.Key == nil {
		return txGet{}, value.Invalid(msgTransactGetNoKey)
	}

	err = value.ValidateItem(get.Key)
	if err != nil {
		return txGet{}, fmt.Errorf("transaction get key: %w", err)
	}

	err = e.data.CheckKey(get.Key)
	if err != nil {
		if refused, ok := errors.AsType[*awserr.Error](err); ok {
			return txGet{}, api.NewTransactionCanceledException(refused.Message)
		}

		return txGet{}, fmt.Errorf("transaction get key: %w", err)
	}

	x, err := newExprs(get.ExpressionAttributeNames, nil, get.ProjectionExpression != nil, false, "")
	if err != nil {
		return txGet{}, err
	}

	projection, err := x.projection(get.ProjectionExpression)
	if err == nil {
		err = x.done()
	}

	if err != nil {
		return txGet{}, fmt.Errorf("transaction get: %w", err)
	}

	return txGet{name: name, entry: e, key: get.Key, projection: projection}, nil
}
