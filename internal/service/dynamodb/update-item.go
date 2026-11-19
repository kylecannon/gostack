package dynamodb

import (
	"context"
	"fmt"
	"maps"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// UpdateItem applies an UpdateExpression to the item with a key, creating it
// from the key when there is none, when its ConditionExpression holds. With no
// UpdateExpression it stores the key alone if nothing is there. The legacy
// AttributeUpdates, Expected and ConditionalOperator answer 501.
func (p *Provider) UpdateItem(ctx context.Context, in *api.UpdateItemInput) (*api.UpdateItemOutput, error) {
	returnOnFailure, err := returnOnConditionFailure(in.ReturnValuesOnConditionCheckFailure)
	if err != nil {
		return nil, err
	}

	e, err := p.caller(ctx).lookup(in.TableName)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	if in.AttributeUpdates != nil || in.Expected != nil || in.ConditionalOperator != nil {
		return nil, notImplemented("UpdateItem")
	}

	mode, err := updateReturnValues(in.ReturnValues)
	if err != nil {
		return nil, err
	}

	err = checkKey(e, in.Key)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	update, cond, err := updateExpressions(in)
	if err != nil {
		return nil, err
	}

	change, err := e.data.Write(in.Key, func(old value.Item) (value.Item, error) {
		failed := checkCondition(cond, old, returnOnFailure)
		if failed != nil {
			return nil, failed
		}

		return applyUpdate(update, in.Key, old)
	})
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	var paths []expression.Path
	if update != nil {
		paths = update.Paths()
	}

	used := writeCapacity(e, change.Old, change.New)

	return &api.UpdateItemOutput{
		Attributes:       expression.ReturnValues(mode, paths, change.Old, change.New),
		ConsumedCapacity: used.report(in.ReturnConsumedCapacity, *in.TableName),
	}, nil
}

// updateExpressions parses an UpdateItem's UpdateExpression, then its
// ConditionExpression, and checks their placeholders, as DynamoDB Local orders
// them.
//
//nolint:ireturn // the parsed condition is the expression package's interface
func updateExpressions(in *api.UpdateItemInput) (*expression.UpdateExpr, expression.Condition, error) {
	hasExpr := in.UpdateExpression != nil || in.ConditionExpression != nil

	x, err := newExprs(in.ExpressionAttributeNames, in.ExpressionAttributeValues, hasExpr, hasExpr,
		"UpdateExpression and ConditionExpression are null")
	if err != nil {
		return nil, nil, err
	}

	update, err := x.update(in.UpdateExpression)
	if err != nil {
		return nil, nil, fmt.Errorf("update: %w", err)
	}

	cond, err := x.condition(expression.ConditionExpression, in.ConditionExpression)
	if err == nil {
		err = x.done()
	}

	if err != nil {
		return nil, nil, fmt.Errorf("update: %w", err)
	}

	return update, cond, nil
}

// applyUpdate is the item an update makes of old: the expression applied, or
// with none, old as it is or the key alone when nothing is stored.
func applyUpdate(update *expression.UpdateExpr, key, old value.Item) (value.Item, error) {
	if update != nil {
		updated, err := update.Apply(key, old)
		if err != nil {
			return nil, fmt.Errorf("update: %w", err)
		}

		return updated, nil
	}

	if old != nil {
		return old, nil
	}

	return maps.Clone(key), nil
}

// updateReturnValues checks UpdateItem's ReturnValues, any of the five.
func updateReturnValues(v *string) (string, error) {
	if v == nil {
		return expression.ReturnNone, nil
	}

	switch *v {
	case expression.ReturnNone, expression.ReturnAllOld, expression.ReturnUpdatedOld,
		expression.ReturnAllNew, expression.ReturnUpdatedNew:
		return *v, nil
	default:
		return "", value.Invalid(msgReturnValues)
	}
}
