package dynamodb

import (
	"context"
	"fmt"
	"maps"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

const msgUpdateNull = "UpdateExpression and ConditionExpression are null"

// UpdateItem applies an UpdateExpression to the item with a key, creating it
// from the key when there is none, when its ConditionExpression, or the legacy
// Expected and ConditionalOperator, hold. With no UpdateExpression it stores
// the key alone if nothing is there. The legacy AttributeUpdates are translated
// into the same update, and checked as DynamoDB Local does: once the condition
// holds, so a failed condition answers before an entry that is refused.
func (p *Provider) UpdateItem(ctx context.Context, in *api.UpdateItemInput) (*api.UpdateItemOutput, error) {
	name, e, err := p.lookupHome(p.caller(ctx), in.TableName)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	plan, err := planUpdate(e, in, isStatement(ctx))
	if err != nil {
		return nil, err
	}

	change, err := e.data.Write(in.Key, func(old value.Item) (value.Item, error) {
		failed := plan.guard.check(old)
		if failed != nil {
			return nil, failed
		}

		if len(in.AttributeUpdates) > 0 {
			update, legacyErr := legacyUpdate(in.AttributeUpdates, in.Key, old)
			if legacyErr != nil {
				return nil, legacyErr
			}

			plan.update = update
		}

		return applyUpdate(plan.update, in.Key, old)
	})
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	var paths []expression.Path
	if plan.update != nil {
		paths = plan.update.Paths()
	}

	used := writeCapacity(e, change.Old, change.New)

	return &api.UpdateItemOutput{
		Attributes:       expression.ReturnValues(plan.mode, paths, change.Old, change.New),
		ConsumedCapacity: used.report(in.ReturnConsumedCapacity, name),
	}, nil
}

// updatePlan is what an UpdateItem has checked before it writes: the update
// its UpdateExpression parsed to, nil with none or with AttributeUpdates,
// which are checked once the condition holds; the condition; and what
// ReturnValues asks to answer.
type updatePlan struct {
	update *expression.UpdateExpr
	guard  writeGuard
	mode   string
}

// planUpdate checks an UpdateItem's parameters in the order DynamoDB Local
// does: the entries of Expected, ReturnValues, the mix with expressions,
// placeholders no expression uses, the key, the ConditionalOperator, the
// expressions, and last the enum of ReturnValuesOnConditionCheckFailure.
func planUpdate(e *tableEntry, in *api.UpdateItemInput, statement bool) (*updatePlan, error) {
	tests, err := expectedTests(in.Expected)
	if err != nil {
		return nil, err
	}

	plan := &updatePlan{}

	plan.mode, err = updateReturnValues(in.ReturnValues)
	if err != nil {
		return nil, err
	}

	err = checkWriteMixed(len(in.AttributeUpdates), in.Expected, in.ConditionalOperator,
		namedExpr{paramUpdate, in.UpdateExpression}, namedExpr{paramCondition, in.ConditionExpression})
	if err != nil {
		return nil, err
	}

	hasExpr := in.UpdateExpression != nil || in.ConditionExpression != nil

	err = checkUsable(in.ExpressionAttributeNames, in.ExpressionAttributeValues, hasExpr, hasExpr, msgUpdateNull)
	if err != nil {
		return nil, err
	}

	err = checkKey(e, in.Key)
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	plan.update, plan.guard.cond, err = updateExpressions(in, tests, statement)
	if err != nil {
		return nil, err
	}

	plan.guard.returnOld, err = returnOnConditionFailure(in.ReturnValuesOnConditionCheckFailure)
	if err != nil {
		return nil, err
	}

	return plan, nil
}

// updateExpressions checks an UpdateItem's ConditionalOperator against the
// tests of its Expected, then parses its UpdateExpression and its
// ConditionExpression and checks their placeholders, as DynamoDB Local orders
// them.
//
//nolint:ireturn // the parsed condition is the expression package's interface
func updateExpressions(
	in *api.UpdateItemInput, tests []expression.LegacyTest, statement bool,
) (*expression.UpdateExpr, expression.Condition, error) {
	legacy, err := expectedCondition(tests, in.ConditionalOperator)
	if err != nil {
		return nil, nil, err
	}

	hasExpr := in.UpdateExpression != nil || in.ConditionExpression != nil

	x, err := newExprs(in.ExpressionAttributeNames, in.ExpressionAttributeValues, hasExpr, hasExpr, msgUpdateNull)
	if err != nil {
		return nil, nil, err
	}

	x.statement = statement

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

	if legacy != nil {
		cond = legacy
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
