package dynamodb

import (
	"context"
	"fmt"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

const (
	msgNullItem        = "Item to put in PutItem cannot be null"
	msgNullKey         = "Cannot have null key for GetItem, DeleteItem, or UpdateItem"
	msgConditionFailed = "The conditional request failed"
)

// The ReturnValues PutItem and DeleteItem take, and the
// ReturnValuesOnConditionCheckFailure values.
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

// PutItem stores an item, replacing the one with its key, when its
// ConditionExpression holds. The legacy Expected and ConditionalOperator
// answer 501.
func (p *Provider) PutItem(ctx context.Context, in *api.PutItemInput) (*api.PutItemOutput, error) {
	returnOnFailure, err := returnOnConditionFailure(in.ReturnValuesOnConditionCheckFailure)
	if err != nil {
		return nil, err
	}

	e, err := p.caller(ctx).lookup(in.TableName)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	if in.Expected != nil || in.ConditionalOperator != nil {
		return nil, notImplemented("PutItem")
	}

	returnOld, err := returnValues(in.ReturnValues)
	if err != nil {
		return nil, err
	}

	if in.Item == nil {
		return nil, value.Invalid(msgNullItem)
	}

	err = value.ValidateItem(in.Item)
	if err == nil {
		err = e.data.CheckItem(in.Item)
	}

	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	cond, err := writeCondition(in.ConditionExpression, in.ExpressionAttributeNames, in.ExpressionAttributeValues)
	if err != nil {
		return nil, err
	}

	change, err := e.data.Write(in.Item, func(old value.Item) (value.Item, error) {
		return in.Item, checkCondition(cond, old, returnOnFailure)
	})
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	used := writeCapacity(e, change.Old, change.New)
	out := &api.PutItemOutput{ConsumedCapacity: used.report(in.ReturnConsumedCapacity, *in.TableName)}

	if returnOld {
		out.Attributes = change.Old
	}

	return out, nil
}

// GetItem reads the item with a key, projected by its ProjectionExpression.
// The legacy AttributesToGet answers 501.
func (p *Provider) GetItem(ctx context.Context, in *api.GetItemInput) (*api.GetItemOutput, error) {
	// DynamoDB Local refuses names with no expression before it looks for the
	// table, here alone.
	x, err := newExprs(in.ExpressionAttributeNames, nil, in.ProjectionExpression != nil, false, "")
	if err != nil {
		return nil, err
	}

	e, err := p.caller(ctx).lookup(in.TableName)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	if in.AttributesToGet != nil {
		return nil, notImplemented("GetItem")
	}

	err = checkKey(e, in.Key)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	projection, err := x.projection(in.ProjectionExpression)
	if err == nil {
		err = x.done()
	}

	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	stored := e.data.Get(in.Key)

	item := stored
	if projection != nil && stored != nil {
		item = projection.Apply(stored)
	}

	used := consumption{table: readUnits(value.ItemSize(stored), in.ConsistentRead)}

	return &api.GetItemOutput{Item: item, ConsumedCapacity: used.report(in.ReturnConsumedCapacity, *in.TableName)}, nil
}

// DeleteItem removes the item with a key, if there is one, when its
// ConditionExpression holds. The legacy Expected and ConditionalOperator
// answer 501.
func (p *Provider) DeleteItem(ctx context.Context, in *api.DeleteItemInput) (*api.DeleteItemOutput, error) {
	returnOnFailure, err := returnOnConditionFailure(in.ReturnValuesOnConditionCheckFailure)
	if err != nil {
		return nil, err
	}

	e, err := p.caller(ctx).lookup(in.TableName)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	if in.Expected != nil || in.ConditionalOperator != nil {
		return nil, notImplemented("DeleteItem")
	}

	returnOld, err := returnValues(in.ReturnValues)
	if err != nil {
		return nil, err
	}

	err = checkKey(e, in.Key)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	cond, err := writeCondition(in.ConditionExpression, in.ExpressionAttributeNames, in.ExpressionAttributeValues)
	if err != nil {
		return nil, err
	}

	change, err := e.data.Write(in.Key, func(old value.Item) (value.Item, error) {
		// nil deletes, as Write takes it.
		return nil, checkCondition(cond, old, returnOnFailure)
	})
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	used := writeCapacity(e, change.Old, nil)
	out := &api.DeleteItemOutput{ConsumedCapacity: used.report(in.ReturnConsumedCapacity, *in.TableName)}

	if returnOld {
		out.Attributes = change.Old
	}

	return out, nil
}

// writeCondition parses the ConditionExpression of a PutItem or DeleteItem
// and checks its placeholders; nil means no condition.
//
//nolint:ireturn // the parsed form is the expression package's interface
func writeCondition(
	src *string, names map[string]string, values map[string]api.AttributeValue,
) (expression.Condition, error) {
	x, err := newExprs(names, values, src != nil, src != nil, "ConditionExpression is null")
	if err != nil {
		return nil, err
	}

	cond, err := x.condition(expression.ConditionExpression, src)
	if err == nil {
		err = x.done()
	}

	if err != nil {
		return nil, fmt.Errorf("condition: %w", err)
	}

	return cond, nil
}

// checkCondition evaluates a write's condition on the stored item, old, and
// refuses the write when it does not hold: with the stored item in the error
// when ReturnValuesOnConditionCheckFailure asked for it and there is one.
//
//nolint:revive // flag-parameter: what the request asked for, not a mode
func checkCondition(cond expression.Condition, old value.Item, returnOld bool) error {
	if cond == nil {
		return nil
	}

	ok, err := cond.Eval(old)
	if err != nil {
		return fmt.Errorf("condition: %w", err)
	}

	if ok {
		return nil
	}

	if returnOld && old != nil {
		return (&api.ConditionalCheckFailedException{Item: old}).Err(msgConditionFailed)
	}

	return api.NewConditionalCheckFailedException(msgConditionFailed)
}

// returnOnConditionFailure reports whether ReturnValuesOnConditionCheckFailure
// asks for the stored item. Its enum is checked as the model checks it, before
// anything else.
func returnOnConditionFailure(v *string) (bool, error) {
	switch {
	case v == nil || *v == returnNone:
		return false, nil
	case *v == returnAllOld:
		return true, nil
	default:
		return false, enumError("returnValuesOnConditionCheckFailure", *v, "ALL_OLD, NONE")
	}
}

// checkKey checks a key given to GetItem, DeleteItem or UpdateItem: present,
// valid values, and the table's key attributes exactly.
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

// readUnits is what a read of size bytes costs: a unit per 4 KB begun, at
// least one, halved for an eventually consistent read.
func readUnits(size int, consistentRead *bool) float64 {
	units := float64(unitsOf(size, readUnitBytes))
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
