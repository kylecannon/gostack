package dynamodb

import (
	"context"
	"fmt"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

const (
	msgNullItem = "Item to put in PutItem cannot be null"
	msgNullKey  = "Cannot have null key for GetItem, DeleteItem, or UpdateItem"
)

// The ReturnValues PutItem and DeleteItem take.
const (
	returnNone   = "NONE"
	returnAllOld = "ALL_OLD"
)

// The ReturnConsumedCapacity values that ask for it.
const (
	consumedTotal   = "TOTAL"
	consumedIndexes = "INDEXES"
)

// Capacity unit sizes, in bytes.
const (
	readUnitBytes  = 4096
	writeUnitBytes = 1024
)

// PutItem stores an item, replacing the one with its key. Conditions answer
// 501 until the condition expression change brings them.
func (p *Provider) PutItem(ctx context.Context, in *api.PutItemInput) (*api.PutItemOutput, error) {
	c := p.caller(ctx)

	e, err := c.lookup(in.TableName)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	if in.ConditionExpression != nil || in.Expected != nil || in.ConditionalOperator != nil ||
		in.ExpressionAttributeNames != nil || in.ExpressionAttributeValues != nil {
		return nil, notImplemented("PutItem")
	}

	if in.Item == nil {
		return nil, value.Invalid(msgNullItem)
	}

	returnOld, err := returnValues(in.ReturnValues)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	err = value.ValidateItem(in.Item)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	err = e.data.CheckItem(in.Item)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	change, err := e.data.Write(in.Item, func(value.Item) (value.Item, error) { return in.Item, nil })
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	out := &api.PutItemOutput{
		ConsumedCapacity: consumed(in.ReturnConsumedCapacity, *in.TableName, writeUnits(change.Old, change.New)),
	}
	if returnOld {
		out.Attributes = change.Old
	}

	return out, nil
}

// GetItem reads the item with a key. A projection answers 501 until the
// projection expression change brings it.
func (p *Provider) GetItem(ctx context.Context, in *api.GetItemInput) (*api.GetItemOutput, error) {
	c := p.caller(ctx)

	e, err := c.lookup(in.TableName)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	if in.ProjectionExpression != nil || in.AttributesToGet != nil || in.ExpressionAttributeNames != nil {
		return nil, notImplemented("GetItem")
	}

	err = checkKey(e, in.Key)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	item := e.data.Get(in.Key)

	return &api.GetItemOutput{
		Item:             item,
		ConsumedCapacity: consumed(in.ReturnConsumedCapacity, *in.TableName, readUnits(item, in.ConsistentRead)),
	}, nil
}

// DeleteItem removes the item with a key, if there is one. Conditions answer
// 501 until the condition expression change brings them.
func (p *Provider) DeleteItem(ctx context.Context, in *api.DeleteItemInput) (*api.DeleteItemOutput, error) {
	c := p.caller(ctx)

	e, err := c.lookup(in.TableName)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	if in.ConditionExpression != nil || in.Expected != nil || in.ConditionalOperator != nil ||
		in.ExpressionAttributeNames != nil || in.ExpressionAttributeValues != nil {
		return nil, notImplemented("DeleteItem")
	}

	returnOld, err := returnValues(in.ReturnValues)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	err = checkKey(e, in.Key)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	// nil deletes, as Write takes it.
	remove := func(value.Item) (value.Item, error) { return nil, nil } //nolint:nilnil // see above

	change, err := e.data.Write(in.Key, remove)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	out := &api.DeleteItemOutput{
		ConsumedCapacity: consumed(in.ReturnConsumedCapacity, *in.TableName, writeUnits(change.Old, nil)),
	}
	if returnOld {
		out.Attributes = change.Old
	}

	return out, nil
}

// checkKey checks a key given to GetItem or DeleteItem: present, valid values,
// and the table's key attributes exactly.
func checkKey(e *tableEntry, key value.Item) error {
	if key == nil {
		return value.Invalid(msgNullKey)
	}

	err := value.ValidateItem(key)
	if err != nil {
		return fmt.Errorf("item key: %w", err)
	}

	err = e.data.CheckKey(key)
	if err != nil {
		return fmt.Errorf("item key: %w", err)
	}

	return nil
}

// returnValues reports whether ReturnValues asks for the old item: NONE and
// ALL_OLD are the values PutItem and DeleteItem take.
func returnValues(v *string) (bool, error) {
	switch {
	case v == nil || *v == returnNone:
		return false, nil
	case *v == returnAllOld:
		return true, nil
	default:
		return false, value.Invalid(msgReturnValues)
	}
}

// consumed is the ConsumedCapacity ReturnConsumedCapacity asks for, or nil.
func consumed(mode *string, tableName string, units float64) *api.ConsumedCapacity {
	if mode == nil {
		return nil
	}

	switch *mode {
	case consumedTotal:
		return &api.ConsumedCapacity{TableName: new(tableName), CapacityUnits: new(units)}
	case consumedIndexes:
		return &api.ConsumedCapacity{
			TableName: new(tableName), CapacityUnits: new(units), Table: &api.Capacity{CapacityUnits: new(units)},
		}
	default:
		return nil
	}
}

// readUnits is what a read of item costs: a unit per 4 KB begun, at least
// one, halved for an eventually consistent read.
func readUnits(item value.Item, consistentRead *bool) float64 {
	units := float64(unitsOf(value.ItemSize(item), readUnitBytes))
	if consistentRead == nil || !*consistentRead {
		units /= 2
	}

	return units
}

// writeUnits is what a write costs: a unit per 1 KB begun of the larger of the
// old and the new item, at least one.
func writeUnits(old, updated value.Item) float64 {
	return float64(unitsOf(max(value.ItemSize(old), value.ItemSize(updated)), writeUnitBytes))
}

func unitsOf(size, unit int) int {
	return max(1, (size+unit-1)/unit)
}
