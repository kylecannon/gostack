package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/partiql"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The messages ExecuteTransaction is refused with.
const (
	msgTxEmpty     = "ExecutePartiQLTransaction can not have empty statement."
	msgTxTooMany   = "Member must have length less than or equal to 100"
	msgTxMixed     = "ExecuteTransaction API does not support both read and write operations in the same request."
	msgTxFailed    = "Validation failed in TransactStatements["
	msgTxReturning = "RETURNING clause is not supported in ExecuteTransaction."
	msgTxSelectKey = "Select statements within ExecuteTransaction must specify the primary key in the where clause."
	msgTxNotFound  = "Requested resource not found"

	msgTxTokenMismatch = "Specified idempotent token was used with different request parameters " +
		"within the idempotency window"
)

// codeTokenMismatch is IdempotentParameterMismatchException's code.
const codeTokenMismatch = "IdempotentParameterMismatchException" //nolint:gosec // an error code, not a credential

// ExecuteTransaction runs up to 100 statements, all reads or all writes, as
// one TransactGetItems or TransactWriteItems: all or nothing, with a
// cancellation reason per statement.
func (p *Provider) ExecuteTransaction(
	ctx context.Context, in *api.ExecuteTransactionInput,
) (*api.ExecuteTransactionOutput, error) {
	stmts, reads, err := parseTransaction(in.TransactStatements)
	if err != nil {
		return nil, err
	}

	if reads {
		return p.transactReads(ctx, in, stmts)
	}

	return p.transactWrites(ctx, in, stmts)
}

// parseTransaction parses a transaction's statements and makes the checks on
// the whole of it. reads is whether they are all SELECTs.
func parseTransaction(reqs []api.ParameterizedStatement) ([]*partiql.Statement, bool, error) {
	switch {
	case len(reqs) == 0:
		return nil, false, value.Invalid(msgTxEmpty)
	case len(reqs) > maxTransactItems:
		return nil, false, value.Invalid(msgTxTooMany)
	default:
	}

	stmts := make([]*partiql.Statement, len(reqs))
	reads := 0

	for i := range reqs {
		stmt, err := parseStatement(reqs[i].Statement, reqs[i].Parameters, true)
		if err != nil {
			return nil, false, err
		}

		if stmt.Returning != partiql.ReturnNone {
			return nil, false, txFailed(i, msgTxReturning)
		}

		if stmt.Kind == partiql.Select {
			reads++
		}

		stmts[i] = stmt
	}

	if reads > 0 && reads < len(stmts) {
		return nil, false, value.Invalid(msgTxMixed)
	}

	return stmts, reads > 0, nil
}

// txFailed is the message for statement i's own failure.
func txFailed(i int, msg string) error {
	return value.Invalid(msgTxFailed + strconv.Itoa(i) + "]: " + msg)
}

// transactReads runs single-item SELECTs as one TransactGetItems.
func (p *Provider) transactReads(
	ctx context.Context, in *api.ExecuteTransactionInput, stmts []*partiql.Statement,
) (*api.ExecuteTransactionOutput, error) {
	c := p.caller(ctx)
	gets := make([]api.TransactGetItem, len(stmts))
	aliases := make([][]partiql.Path, len(stmts))

	for i, stmt := range stmts {
		read, err := c.compileRead(stmt, in.TransactStatements[i].Parameters, nil)
		if errors.Is(err, errNotSingle) {
			return nil, txFailed(i, msgTxSelectKey)
		}

		if err != nil {
			return nil, txStatementError(i, err)
		}

		g := read.get
		gets[i] = api.TransactGetItem{Get: &api.Get{
			TableName: g.TableName, Key: g.Key, ProjectionExpression: g.ProjectionExpression,
			ExpressionAttributeNames: g.ExpressionAttributeNames,
		}}
		aliases[i] = read.aliases
	}

	out, err := p.TransactGetItems(ctx, &api.TransactGetItemsInput{
		TransactItems: gets, ReturnConsumedCapacity: in.ReturnConsumedCapacity,
	})
	if err != nil {
		return nil, fmt.Errorf("transaction: %w", err)
	}

	for i := range out.Responses {
		out.Responses[i].Item = shapeItem(out.Responses[i].Item, aliases[i])
	}

	return &api.ExecuteTransactionOutput{Responses: out.Responses, ConsumedCapacity: out.ConsumedCapacity}, nil
}

// txNotFound names a missing table as a transaction does.
func txNotFound(err error) error {
	if isCode(err, codeNotFound) {
		return api.NewResourceNotFoundException(msgTxNotFound)
	}

	return err
}

// transactWrites runs INSERT, UPDATE, DELETE and EXISTS statements as one
// TransactWriteItems: a Put that holds when the key is new, an Update, a
// Delete and a ConditionCheck.
func (p *Provider) transactWrites(
	ctx context.Context, in *api.ExecuteTransactionInput, stmts []*partiql.Statement,
) (*api.ExecuteTransactionOutput, error) {
	c := p.caller(ctx)
	items := make([]api.TransactWriteItem, len(stmts))

	for i, stmt := range stmts {
		req := &in.TransactStatements[i]

		returnOld, err := returnOnConditionFailure(req.ReturnValuesOnConditionCheckFailure)
		if err != nil {
			return nil, err
		}

		w, err := c.compileWrite(stmt, req.Parameters, returnOld)
		if err != nil {
			return nil, txCompileError(i, len(stmts), err)
		}

		items[i] = w.transactItem()
	}

	out, err := p.TransactWriteItems(ctx, &api.TransactWriteItemsInput{
		TransactItems: items, ClientRequestToken: in.ClientRequestToken,
		ReturnConsumedCapacity: in.ReturnConsumedCapacity,
	})

	if cancel, ok := errors.AsType[*canceledError](err); ok {
		return nil, transactionCanceled(insertReasons(stmts, cancel.reasons))
	}

	// TransactWriteItems answers a reused token with no message, and
	// ExecuteTransaction with one (verification/SQ-229 probe2).
	if isCode(err, codeTokenMismatch) {
		return nil, api.NewIdempotentParameterMismatchException(msgTxTokenMismatch)
	}

	if err != nil {
		return nil, fmt.Errorf("transaction: %w", err)
	}

	return &api.ExecuteTransactionOutput{Responses: []api.ItemResponse{}, ConsumedCapacity: out.ConsumedCapacity}, nil
}

// txCompileError answers statement i of n failing to compile, as DynamoDB
// Local does: a WHERE without the whole key cancels the transaction with no
// reasons, an EXISTS without a condition fails the request, and a key of the
// wrong shape is the statement's cancellation reason.
func txCompileError(i, n int, err error) error {
	var shape *shapeError

	switch {
	case errors.As(err, &shape):
		return api.NewTransactionCanceledException(reasonValidation + " " + messageOf(err))
	case isCode(err, codeValidation) && messageOf(err) == msgExistsCondition:
		return txFailed(i, msgExistsCondition)
	case isCode(err, codeValidation) && isKeyMessage(messageOf(err)):
		reasons := make([]api.CancellationReason, n)
		for j := range reasons {
			reasons[j] = cancellation(reasonNone, "", nil)
		}

		reasons[i] = cancellation(reasonValidation, messageOf(err), nil)

		return transactionCanceled(reasons)
	default:
		return txStatementError(i, err)
	}
}

// txStatementError answers statement i failing to compile: a validation
// error names the statement, without the prefix of the expression it came
// from, and a missing table is the transaction's not-found
// (verification/SQ-229 probe5).
func txStatementError(i int, err error) error {
	if isCode(err, codeValidation) {
		return txFailed(i, messageOf(statementFailure(err)))
	}

	return txNotFound(err)
}

// isKeyMessage reports whether msg refuses an item key's shape.
func isKeyMessage(msg string) bool {
	return strings.HasPrefix(msg, msgKeyAbsent) || strings.HasPrefix(msg, msgStatementKeyType)
}

// insertReasons names an INSERT's failed condition as DynamoDB does: the
// key is already there.
func insertReasons(stmts []*partiql.Statement, reasons []api.CancellationReason) []api.CancellationReason {
	for i, r := range reasons {
		if stmts[i].Kind == partiql.Insert && r.Code != nil && *r.Code == reasonCondition {
			reasons[i] = cancellation(reasonValidation, msgDuplicateItem, nil)
		}
	}

	return reasons
}

// transactItem is the compiled write as an action of TransactWriteItems.
func (w *writeStatement) transactItem() api.TransactWriteItem {
	switch {
	case w.put != nil:
		return api.TransactWriteItem{Put: &api.Put{
			TableName: w.put.TableName, Item: w.put.Item, ConditionExpression: w.put.ConditionExpression,
			ExpressionAttributeNames: w.put.ExpressionAttributeNames,
		}}
	case w.update != nil:
		u := w.update

		return api.TransactWriteItem{Update: &api.Update{
			TableName: u.TableName, Key: u.Key, UpdateExpression: u.UpdateExpression,
			ConditionExpression: u.ConditionExpression, ExpressionAttributeNames: u.ExpressionAttributeNames,
			ExpressionAttributeValues:           u.ExpressionAttributeValues,
			ReturnValuesOnConditionCheckFailure: u.ReturnValuesOnConditionCheckFailure,
		}}
	case w.del != nil:
		d := w.del

		return api.TransactWriteItem{Delete: &api.Delete{
			TableName: d.TableName, Key: d.Key, ConditionExpression: d.ConditionExpression,
			ExpressionAttributeNames: d.ExpressionAttributeNames, ExpressionAttributeValues: d.ExpressionAttributeValues,
			ReturnValuesOnConditionCheckFailure: d.ReturnValuesOnConditionCheckFailure,
		}}
	default:
		return api.TransactWriteItem{ConditionCheck: w.check}
	}
}

// messageOf is the message of an AWS error, or its text.
func messageOf(err error) string {
	if shape, ok := errors.AsType[*shapeError](err); ok {
		err = shape.err
	}

	ae := asAWSError(err)
	if ae != nil {
		return ae.Message
	}

	return err.Error()
}
