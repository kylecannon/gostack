package dynamodb

import (
	"context"
	"fmt"

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

	plan, err := planUpdate(originOf(ctx), e, in)
	if err != nil {
		return nil, err
	}

	change, err := e.data.WritePacked(in.Key, func(stored value.Packed) (value.Item, value.Packed, error) {
		// The item stored is unpacked once, for the condition and the legacy
		// AttributeUpdates that read it, and the update writes into it. The
		// attributes it writes are spliced into the stored item's bytes, so
		// the ones it leaves are not packed again.
		read := func(old value.Item) (*expression.UpdateExpr, error) {
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

			return plan.update, nil
		}

		updated, packed, applyErr := expression.ApplyStoredPacked(in.Key, stored, read)
		if applyErr != nil {
			return nil, "", fmt.Errorf("update: %w", applyErr)
		}

		return updated, packed, nil
	})
	if err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}

	var paths []expression.Path
	if plan.update != nil {
		paths = plan.update.Paths()
	}

	used := writeCapacity(e, change.OldPacked, change.New)

	// change.New may be the item the update unpacked and wrote into, so the
	// old item is unpacked again from what was stored, and only for the
	// ReturnValues that answer it.
	var old value.Item
	if plan.mode == expression.ReturnAllOld || plan.mode == expression.ReturnUpdatedOld {
		old = value.Unpack(change.OldPacked)
	}

	return &api.UpdateItemOutput{
		Attributes:       expression.ReturnValues(plan.mode, paths, old, change.New),
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
func planUpdate(from origin, e *tableEntry, in *api.UpdateItemInput) (*updatePlan, error) {
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

	plan.update, plan.guard.cond, err = updateExpressions(from, in, tests)
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
	from origin, in *api.UpdateItemInput, tests []expression.LegacyTest,
) (*expression.UpdateExpr, expression.Condition, error) {
	legacy, err := expectedCondition(tests, in.ConditionalOperator)
	if err != nil {
		return nil, nil, err
	}

	hasExpr := in.UpdateExpression != nil || in.ConditionExpression != nil

	x, err := newExprs(from, in.ExpressionAttributeNames, in.ExpressionAttributeValues, hasExpr, hasExpr, msgUpdateNull)
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

	if legacy != nil {
		cond = legacy
	}

	return update, cond, nil
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
