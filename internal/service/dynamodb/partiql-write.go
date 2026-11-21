package dynamodb

import (
	"context"
	"errors"
	"fmt"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/kylecannon/gostack/internal/service/dynamodb/partiql"
	"github.com/kylecannon/gostack/internal/service/dynamodb/table"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// msgStatementEmpty refuses a request with no statement.
const (
	msgStatementEmpty  = "The statement was empty."
	msgExistsCondition = "EXISTS() must contain a single item read with additional condition"
)

// conditionFailedError is a ConditionalCheckFailedException that carries the item
// it returns, so a PartiQL batch can put that item in the statement's error.
type conditionFailedError struct {
	item value.Item
	err  error
}

func (e *conditionFailedError) Error() string { return e.err.Error() }

func (e *conditionFailedError) Unwrap() error { return e.err }

// canceledError is a TransactionCanceledException that carries its reasons, so
// ExecuteTransaction can name a failed INSERT's reason as DynamoDB does.
type canceledError struct {
	reasons []api.CancellationReason
	err     error
}

func (e *canceledError) Error() string { return e.err.Error() }

func (e *canceledError) Unwrap() error { return e.err }

// writeStatement is an INSERT, UPDATE, DELETE or EXISTS compiled to the item
// operation it runs as: exactly one of put, update, del and check is set.
type writeStatement struct {
	kind   partiql.StatementKind
	table  string
	key    value.Item
	put    *api.PutItemInput
	update *api.UpdateItemInput
	// rank is where each action of update's expression stands in the UPDATE
	// statement, which lists them in a different order.
	rank  []int
	del   *api.DeleteItemInput
	check *api.ConditionCheck
}

// compileWrite compiles a write statement against its table. returnOld is
// ReturnValuesOnConditionCheckFailure's ALL_OLD.
//
//nolint:revive // flag-parameter: what the request asked for, not a mode
func (c caller) compileWrite(
	stmt *partiql.Statement, params []api.AttributeValue, returnOld bool,
) (*writeStatement, error) {
	name, e, err := c.statementTable(&stmt.Table)
	if err != nil {
		// DynamoDB Local looks an INSERT's table up without checking its name.
		if stmt.Kind == partiql.Insert && isCode(err, codeValidation) {
			return nil, api.NewResourceNotFoundException(msgTableNotFound)
		}

		return nil, fmt.Errorf("statement: %w", err)
	}

	key := e.data.Key()
	r := newRender(params)
	w := &writeStatement{kind: stmt.Kind, table: name}

	if stmt.Kind == partiql.Insert {
		return w, w.compileInsert(stmt, key, r)
	}

	var rest []*partiql.Node

	w.key, rest, err = singleItem(stmt.Where, key, r)
	if err != nil {
		return nil, err
	}

	cond, err := renderCond(joinAll(rest), r)
	if err != nil {
		return nil, err
	}

	var onFailure *string
	if returnOld {
		onFailure = new(returnAllOld)
	}

	switch stmt.Kind {
	case partiql.Update:
		err = w.compileUpdate(stmt, key.Hash.Name, cond, onFailure, r)
	case partiql.Delete:
		w.del = &api.DeleteItemInput{
			TableName: &w.table, Key: w.key, ConditionExpression: cond, ReturnValuesOnConditionCheckFailure: onFailure,
			ExpressionAttributeNames: r.attributeNames(), ExpressionAttributeValues: r.attributeValues(),
		}
		if stmt.Returning == partiql.ReturnAllOld {
			w.del.ReturnValues = new(returnAllOld)
		}
	default: // partiql.Exists
		if cond == nil {
			return nil, value.Invalid(msgExistsCondition)
		}

		// DynamoDB Local returns no item for a failed EXISTS, whatever
		// ReturnValuesOnConditionCheckFailure asks (verification/SQ-229
		// probe2, "tx exists fail").
		w.check = &api.ConditionCheck{
			TableName: &w.table, Key: w.key, ConditionExpression: cond,
			ExpressionAttributeNames: r.attributeNames(), ExpressionAttributeValues: r.attributeValues(),
		}
	}

	return w.checked(err)
}

// checked is w when it compiled, err nil, and its expressions parse.
func (w *writeStatement) checked(err error) (*writeStatement, error) {
	if err == nil {
		err = w.checkExpressions()
	}

	if err != nil {
		return nil, err
	}

	return w, nil
}

// checkExpressions parses the expressions a compiled UPDATE, DELETE or
// EXISTS runs with, so a statement's own mistake is refused while it is
// compiled: before a batch runs it, and named by its index in a transaction.
func (w *writeStatement) checkExpressions() error {
	var (
		cond, update *string
		names        map[string]string
		values       map[string]api.AttributeValue
	)

	switch {
	case w.update != nil:
		cond, update, names, values = w.update.ConditionExpression, w.update.UpdateExpression,
			w.update.ExpressionAttributeNames, w.update.ExpressionAttributeValues
	case w.del != nil:
		cond, names, values = w.del.ConditionExpression, w.del.ExpressionAttributeNames, w.del.ExpressionAttributeValues
	case w.check != nil:
		cond, names, values = w.check.ConditionExpression, w.check.ExpressionAttributeNames,
			w.check.ExpressionAttributeValues
	default:
		return nil
	}

	return checkStatementExprs(names, values, cond, update, w.rank)
}

// checkStatementExprs parses a compiled statement's condition and update
// expressions over its placeholders. rank is where the update's actions stand
// in the statement.
func checkStatementExprs(
	names map[string]string, values map[string]api.AttributeValue, cond, update *string, rank []int,
) error {
	x, err := newExprs(names, values, true, true, "")
	if err != nil {
		return err
	}

	x.statement = true

	_, err = x.updateRanked(update, rank)
	if err != nil {
		return err
	}

	_, err = x.condition(expression.ConditionExpression, cond)

	return err
}

// codeValidation is ValidationException's code.
const codeValidation = "ValidationException"

// compileInsert compiles an INSERT to a PutItem that holds when no item has
// its key.
func (w *writeStatement) compileInsert(stmt *partiql.Statement, key table.KeySchema, r *render) error {
	item, err := partiql.Value(stmt.Value, r.params, "")
	if err != nil {
		return statementError(err)
	}

	err = checkKeyTypes(item.M, key, true)
	if err != nil {
		return err
	}

	w.key = value.Item{}
	for _, a := range keyParts(key) {
		w.key[a.Name] = item.M[a.Name]
	}

	cond := "attribute_not_exists(" + r.path(partiql.Path{{Name: key.Hash.Name}}) + ")"
	w.put = &api.PutItemInput{
		TableName: &w.table, Item: item.M, ConditionExpression: &cond, ExpressionAttributeNames: r.attributeNames(),
	}

	return nil
}

// returnModes are UpdateItem's ReturnValues for each RETURNING clause.
var returnModes = map[partiql.Returning]string{
	partiql.ReturnAllOld: returnAllOld, partiql.ReturnModifiedOld: "UPDATED_OLD",
	partiql.ReturnAllNew: "ALL_NEW", partiql.ReturnModifiedNew: "UPDATED_NEW",
}

// compileUpdate compiles an UPDATE to an UpdateItem that holds when the item
// exists and the rest of the WHERE clause, cond, holds.
func (w *writeStatement) compileUpdate(
	stmt *partiql.Statement, hash string, cond, onFailure *string, r *render,
) error {
	update, rank, err := r.update(stmt.Actions)
	if err != nil {
		return err
	}

	w.rank = rank

	exists := "attribute_exists(" + r.path(partiql.Path{{Name: hash}}) + ")"
	if cond != nil {
		exists += " AND (" + *cond + ")"
	}

	w.update = &api.UpdateItemInput{
		TableName: &w.table, Key: w.key, UpdateExpression: &update, ConditionExpression: &exists,
		ReturnValuesOnConditionCheckFailure: onFailure, ExpressionAttributeNames: r.attributeNames(),
		ExpressionAttributeValues: r.attributeValues(),
	}

	if mode, ok := returnModes[stmt.Returning]; ok {
		w.update.ReturnValues = &mode
	}

	return nil
}

// runWrite runs a compiled write through its item operation and returns the
// item RETURNING asks for. A failed INSERT condition is a duplicate key.
func (p *Provider) runWrite(
	ctx context.Context, w *writeStatement, capacity *string,
) (value.Item, *api.ConsumedCapacity, error) {
	switch {
	case w.put != nil:
		w.put.ReturnConsumedCapacity = capacity

		out, err := p.PutItem(ctx, w.put)
		if isCode(err, codeConditionFailed) {
			return nil, nil, api.NewDuplicateItemException(msgDuplicateItem)
		}

		if err != nil {
			return nil, nil, fmt.Errorf("statement: %w", err)
		}

		return nil, out.ConsumedCapacity, nil
	case w.update != nil:
		w.update.ReturnConsumedCapacity = capacity

		out, err := p.UpdateItem(ctx, w.update)
		if err != nil {
			return nil, nil, fmt.Errorf("statement: %w", err)
		}

		return out.Attributes, out.ConsumedCapacity, nil
	case w.del != nil:
		w.del.ReturnConsumedCapacity = capacity

		out, err := p.DeleteItem(ctx, w.del)
		if err != nil {
			return nil, nil, fmt.Errorf("statement: %w", err)
		}

		return out.Attributes, out.ConsumedCapacity, nil
	default:
		return nil, nil, notImplemented("ExecuteStatement")
	}
}

// failedItem is the item a failed condition returned, nil when none.
func failedItem(err error) value.Item {
	if cf, ok := errors.AsType[*conditionFailedError](err); ok {
		return cf.item
	}

	return nil
}
