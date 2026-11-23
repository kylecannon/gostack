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
// ConditionExpression, or the legacy Expected and ConditionalOperator, hold.
func (p *Provider) PutItem(ctx context.Context, in *api.PutItemInput) (*api.PutItemOutput, error) {
	name, e, err := p.lookupHome(p.caller(ctx), in.TableName)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
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

	guard, err := putGuard(originOf(ctx), in)
	if err != nil {
		return nil, err
	}

	change, err := e.data.Write(in.Item, func(old value.Item) (value.Item, error) {
		return in.Item, guard.check(old)
	})
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	used := writeCapacity(e, change.Old, change.New)
	out := &api.PutItemOutput{ConsumedCapacity: used.report(in.ReturnConsumedCapacity, name)}

	if returnOld {
		out.Attributes = change.Old
	}

	return out, nil
}

// GetItem reads the item with a key, projected by its ProjectionExpression
// or the legacy AttributesToGet.
func (p *Provider) GetItem(ctx context.Context, in *api.GetItemInput) (*api.GetItemOutput, error) {
	// DynamoDB Local refuses AttributesToGet, and names with no expression,
	// before it looks for the table, here alone.
	err := checkMixed(in.AttributesToGet, namedExpr{paramProjection, in.ProjectionExpression})
	if err != nil {
		return nil, err
	}

	legacy, err := attributesToGet(in.AttributesToGet, sameNameTwice)
	if err != nil {
		return nil, err
	}

	x, err := newExprs(originOf(ctx), in.ExpressionAttributeNames, nil, in.ProjectionExpression != nil, false, "")
	if err != nil {
		return nil, err
	}

	name, e, err := p.lookupHome(p.caller(ctx), in.TableName)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	err = checkKey(e, in.Key)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	projection, err := x.projection(in.ProjectionExpression)
	if err == nil {
		err = x.done()
	}

	if projection == nil {
		projection = legacy
	}

	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	stored := e.data.GetPacked(in.Key)
	used := consumption{table: readUnits(stored.Size(), in.ConsistentRead)}
	out := &api.GetItemOutput{ConsumedCapacity: used.report(in.ReturnConsumedCapacity, name)}

	// An item with nothing to project goes out as it is stored.
	switch {
	case stored == "":
	case projection != nil:
		pj := newProjector(projection)
		out.Item = pj.applyStored(stored)
	default:
		out.ItemJSON = stored.Encoded()
	}

	return out, nil
}

// DeleteItem removes the item with a key, if there is one, when its
// ConditionExpression, or the legacy Expected and ConditionalOperator, hold.
func (p *Provider) DeleteItem(ctx context.Context, in *api.DeleteItemInput) (*api.DeleteItemOutput, error) {
	name, e, err := p.lookupHome(p.caller(ctx), in.TableName)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	guard, returnOld, err := deleteGuard(originOf(ctx), e, in)
	if err != nil {
		return nil, err
	}

	change, err := e.data.Write(in.Key, func(old value.Item) (value.Item, error) {
		// nil deletes, as Write takes it.
		return nil, guard.check(old)
	})
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	used := writeCapacity(e, change.Old, nil)
	out := &api.DeleteItemOutput{ConsumedCapacity: used.report(in.ReturnConsumedCapacity, name)}

	if returnOld {
		out.Attributes = change.Old
	}

	return out, nil
}

// writeGuard is what may stop a PutItem or DeleteItem: its condition, nil for
// none, and whether a failed one reports the item stored.
type writeGuard struct {
	cond      expression.Condition
	returnOld bool
}

// check evaluates the condition on old, the item stored.
func (g *writeGuard) check(old value.Item) error {
	return checkCondition(g.cond, old, g.returnOld)
}

// putGuard checks the condition parameters of a PutItem, in the order DynamoDB
// Local does: the mix of Expected with expressions, placeholders no expression
// uses, the entries of Expected, the enum of ReturnValuesOnConditionCheckFailure,
// and then the ConditionalOperator and the ConditionExpression.
func putGuard(from origin, in *api.PutItemInput) (*writeGuard, error) {
	err := checkWriteMixed(0, in.Expected, in.ConditionalOperator, namedExpr{paramCondition, in.ConditionExpression})
	if err == nil {
		err = checkConditionUsable(in.ConditionExpression, in.ExpressionAttributeNames, in.ExpressionAttributeValues)
	}

	if err != nil {
		return nil, err
	}

	tests, err := expectedTests(in.Expected)
	if err != nil {
		return nil, err
	}

	g := &writeGuard{}

	g.returnOld, err = returnOnConditionFailure(in.ReturnValuesOnConditionCheckFailure)
	if err != nil {
		return nil, err
	}

	return g, g.condition(from, tests, in.ConditionalOperator, in.ConditionExpression,
		in.ExpressionAttributeNames, in.ExpressionAttributeValues)
}

// deleteGuard checks the condition parameters of a DeleteItem, in the order
// DynamoDB Local does: the entries of Expected, ReturnValues, the mix with
// expressions, placeholders no expression uses, the enum of
// ReturnValuesOnConditionCheckFailure, the key, and then the
// ConditionalOperator and the ConditionExpression. It reports whether
// ReturnValues asks for the item deleted.
func deleteGuard(from origin, e *tableEntry, in *api.DeleteItemInput) (*writeGuard, bool, error) {
	tests, err := expectedTests(in.Expected)
	if err != nil {
		return nil, false, err
	}

	returnOld, err := returnValues(in.ReturnValues)
	if err != nil {
		return nil, false, err
	}

	err = checkWriteMixed(0, in.Expected, in.ConditionalOperator, namedExpr{paramCondition, in.ConditionExpression})
	if err == nil {
		err = checkConditionUsable(in.ConditionExpression, in.ExpressionAttributeNames, in.ExpressionAttributeValues)
	}

	if err != nil {
		return nil, false, err
	}

	g := &writeGuard{}

	g.returnOld, err = returnOnConditionFailure(in.ReturnValuesOnConditionCheckFailure)
	if err != nil {
		return nil, false, err
	}

	err = checkKey(e, in.Key)
	if err != nil {
		return nil, false, fmt.Errorf("item: %w", err)
	}

	err = g.condition(from, tests, in.ConditionalOperator, in.ConditionExpression,
		in.ExpressionAttributeNames, in.ExpressionAttributeValues)

	return g, returnOld, err
}

// condition sets the guard's condition: the legacy one made of the tests of
// Expected and the ConditionalOperator, or else the ConditionExpression.
func (g *writeGuard) condition(
	from origin, tests []expression.LegacyTest, op, src *string, names map[string]string,
	values map[string]api.AttributeValue,
) error {
	legacy, err := expectedCondition(tests, op)
	if err != nil {
		return err
	}

	g.cond, err = writeCondition(from, src, names, values)
	if legacy != nil {
		g.cond = legacy
	}

	return err
}

const msgConditionNull = "ConditionExpression is null"

// checkConditionUsable refuses the placeholders of a PutItem or DeleteItem
// that gives no ConditionExpression to use them.
func checkConditionUsable(src *string, names map[string]string, values map[string]api.AttributeValue) error {
	return checkUsable(names, values, src != nil, src != nil, msgConditionNull)
}

// writeCondition parses the ConditionExpression of a PutItem or DeleteItem
// and checks its placeholders; nil means no condition.
//
//nolint:ireturn // the parsed form is the expression package's interface
func writeCondition(
	from origin, src *string, names map[string]string, values map[string]api.AttributeValue,
) (expression.Condition, error) {
	x, err := newExprs(from, names, values, src != nil, src != nil, msgConditionNull)
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
		err := (&api.ConditionalCheckFailedException{Item: old}).Err(msgConditionFailed)

		return &conditionFailedError{item: old, err: err}
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
