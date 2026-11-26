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
	msgTxEmpty   = "ExecutePartiQLTransaction can not have empty statement."
	msgTxTooMany = "Member must have length less than or equal to 100"
	msgTxMixed   = "ExecuteTransaction API does not support both read and write operations in the same request."
	msgTxFailed  = "Validation failed in TransactStatements["
	// msgTxReturnValues starts the answer to a statement's unknown
	// ReturnValuesOnConditionCheckFailure; the member follows.
	msgTxReturnValues = "No enum constant software.amazon.awssdk.services.dynamodb.model." +
		"ReturnValuesOnConditionCheckFailure."
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
	ctx = forStatement(ctx)
	stmts, err := parseTransaction(in.TransactStatements)
	if err != nil {
		return nil, err
	}

	if stmts.reads {
		return p.transactReads(ctx, in, stmts)
	}

	return p.transactWrites(ctx, in, stmts)
}

// txStatements are a transaction's parsed statements. A statement that is a
// DISTINCT, whose parameters do not match, or that has a RETURNING clause, is
// refused when its turn to compile comes (refused). The image looks a RETURNING statement's
// table and key up before it refuses the clause, so returning marks those.
type txStatements struct {
	stmts     []*partiql.Statement
	refused   []error
	returning []bool
	// reads is whether they are all SELECTs.
	reads bool
}

// parseTransaction parses a transaction's statements and makes the checks on
// the whole of it, statement by statement in order, as the image does: an empty
// or long statement, a syntax error, and a statement that mixes reads and writes
// with those before it refuse the call where it stands. A DISTINCT, a parameter
// mismatch and a RETURNING clause are left to each statement's turn after that,
// with the rest of what is wrong with it (SQ-467 image-probe.log).
func parseTransaction(reqs []api.ParameterizedStatement) (*txStatements, error) {
	switch {
	case len(reqs) == 0:
		return nil, value.Invalid(msgTxEmpty)
	case len(reqs) > maxTransactItems:
		return nil, value.Invalid(msgTxTooMany)
	default:
	}

	out := &txStatements{
		stmts: make([]*partiql.Statement, len(reqs)), refused: make([]error, len(reqs)),
		returning: make([]bool, len(reqs)),
	}
	mix := mixing{msg: msgTxMixed}

	for i := range reqs {
		err := checkStatementText(reqs[i].Statement, msgTxEmpty)
		if err != nil {
			return nil, err
		}

		stmt, err := parseStatement(reqs[i].Statement, reqs[i].Parameters, true)

		switch {
		case err != nil && stmt != nil:
			// A DISTINCT or a parameter mismatch, of a statement that has a kind.
			out.refused[i] = txFailed(i, messageOf(err))
		case err != nil:
			return nil, err
		case stmt.Returning != partiql.ReturnNone:
			out.refused[i] = txFailed(i, msgTxReturning)
			out.returning[i] = true
		default:
		}

		mixed := mix.check(stmt)
		if mixed != nil {
			return nil, mixed
		}

		out.stmts[i] = stmt
		out.reads = stmt.Kind == partiql.Select
	}

	return out, nil
}

// txFailed is the message for statement i's own failure.
func txFailed(i int, msg string) error {
	return value.Invalid(msgTxFailed + strconv.Itoa(i) + "]: " + msg)
}

// transactReads runs single-item SELECTs as one TransactGetItems.
func (p *Provider) transactReads(
	ctx context.Context, in *api.ExecuteTransactionInput, parsed *txStatements,
) (*api.ExecuteTransactionOutput, error) {
	c := p.caller(ctx)
	gets := make([]api.TransactGetItem, len(parsed.stmts))

	for i, stmt := range parsed.stmts {
		if parsed.refused[i] != nil {
			return nil, parsed.refused[i]
		}

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
	}

	out, err := p.TransactGetItems(ctx, &api.TransactGetItemsInput{
		TransactItems: gets, ReturnConsumedCapacity: in.ReturnConsumedCapacity,
	})
	if err != nil {
		return nil, fmt.Errorf("transaction: %w", err)
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
	ctx context.Context, in *api.ExecuteTransactionInput, parsed *txStatements,
) (*api.ExecuteTransactionOutput, error) {
	c := p.caller(ctx)
	stmts := parsed.stmts
	items := make([]api.TransactWriteItem, len(stmts))

	for i, stmt := range stmts {
		req := &in.TransactStatements[i]

		if parsed.refused[i] != nil {
			return nil, c.refuseTxStatement(parsed, i, len(stmts), req.Parameters)
		}

		// DynamoDB Local reads the statement's ReturnValuesOnConditionCheckFailure
		// last, after the statement compiles (probed, SQ-331), and names the
		// failed enum of its SDK.
		returnOld, enumErr := returnOnConditionFailure(req.ReturnValuesOnConditionCheckFailure)

		w, err := c.compileWrite(stmt, req.Parameters, returnOld)
		if err != nil {
			return nil, txCompileError(i, len(stmts), err)
		}

		if enumErr != nil {
			return nil, txFailed(i, msgTxReturnValues+*req.ReturnValuesOnConditionCheckFailure)
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

// refuseTxStatement is the refusal of statement i of n. The image looks a
// RETURNING statement's table up, and then its whole key in the WHERE clause,
// before it refuses the clause, so a missing table and a partial key answer
// first (verification/SQ-470 r2ord-cand.log "t URN", r3-cand.log "t URNK");
// the rest of the statement is left unchecked, as a key-type error or an
// update of a key attribute does not change the refusal ("t URKEY").
func (c caller) refuseTxStatement(parsed *txStatements, i, n int, params []api.AttributeValue) error {
	if !parsed.returning[i] {
		return parsed.refused[i]
	}

	// The image names the statement's own expressions before any of it, and
	// then an alias, before it refuses the clause (SQ-418 p2a a4).
	err := beforeTable(parsed.stmts[i], params, aliasRefusal(parsed.stmts[i]))
	if err != nil {
		return txCompileError(i, n, err)
	}

	_, err = c.compileWrite(parsed.stmts[i], params, false)
	if _, shaped := errors.AsType[*shapeError](err); shaped || isCode(err, codeNotFound) {
		return txCompileError(i, n, err)
	}

	return parsed.refused[i]
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
