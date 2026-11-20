package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/service/dynamodb/partiql"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The messages BatchExecuteStatement is refused with.
const (
	msgBatchMixed   = "Read and write requests together in the same batch is not supported."
	msgBatchReads   = "BatchGetItem cannot fetch more than 25 items"
	msgBatchWrites  = "BatchWriteItem cannot write more than 25 items"
	msgSingleSelect = "Only single item select is supported"

	msgBatchNotStatement = "Invalid operation. Only select, insert, update and delete operations are valid"
)

// errNotSingle is a SELECT that does not read exactly one item by its key.
var errNotSingle = errors.New("not a single-item select")

// readStatement is a single-item SELECT compiled to a GetItem.
type readStatement struct {
	table   string
	key     value.Item
	get     *api.GetItemInput
	aliases []partiql.Path
}

// compileRead compiles a SELECT that reads one item by its full key and
// nothing else; any other answers errNotSingle.
func (c caller) compileRead(
	stmt *partiql.Statement, params []api.AttributeValue, consistent *bool,
) (*readStatement, error) {
	name, e, err := c.statementTable(&stmt.Table)
	if err != nil {
		return nil, fmt.Errorf("statement: %w", err)
	}

	if stmt.Index != "" {
		return nil, errNotSingle
	}

	r := newRender(params)

	key, rest, err := singleItem(stmt.Where, e.data.Key(), r)

	_, shaped := errors.AsType[*shapeError](err)
	if shaped {
		return nil, errNotSingle
	}

	if err != nil {
		return nil, err
	}

	if len(rest) > 0 {
		// The rest is refused as a filter, after its own errors: an unknown
		// function or a wrong operand is named before the select is refused.
		var filter string

		filter, err = r.cond(joinAll(rest))
		if err != nil {
			return nil, err
		}

		err = checkStatementExprs(r.attributeNames(), r.attributeValues(), &filter, nil)
		if err != nil {
			return nil, err
		}

		return nil, errNotSingle
	}

	projected, aliases, err := projection(stmt.Projection, r)
	if err != nil {
		return nil, err
	}

	return &readStatement{
		table: name, key: key, aliases: aliases,
		get: &api.GetItemInput{
			TableName: &name, Key: key, ProjectionExpression: projected, ConsistentRead: consistent,
			ExpressionAttributeNames: r.attributeNames(),
		},
	}, nil
}

// batchStatement is one statement of a batch: compiled, or the error that
// answers it.
type batchStatement struct {
	read  *readStatement
	write *writeStatement
	err   error
}

// BatchExecuteStatement runs up to 25 statements, all reads or all writes,
// each on one item, each answered by its own result or error.
func (p *Provider) BatchExecuteStatement(
	ctx context.Context, in *api.BatchExecuteStatementInput,
) (*api.BatchExecuteStatementOutput, error) {
	stmts, err := parseBatch(in.Statements)
	if err != nil {
		return nil, err
	}

	c := p.caller(ctx)
	out := &api.BatchExecuteStatementOutput{Responses: make([]api.BatchStatementResponse, len(stmts))}
	seen := map[string]struct{}{}
	wrote := false

	// DynamoDB Local runs the statements in order and refuses the batch at
	// the first that names an item an earlier one named, after running that
	// one too: what ran before stays written (verification/SQ-229 probe5).
	// The gateway saves nothing after a refused call, so the batch reports
	// those writes itself.
	for i, stmt := range stmts {
		b := batchStatement{err: stmt.err}

		var dup bool

		if stmt.stmt != nil {
			key, table := c.compileBatch(&b, stmt.stmt, &in.Statements[i])
			if key != nil {
				k := itemKeyString(table, key)
				_, dup = seen[k]
				seen[k] = struct{}{}
			}
		}

		out.Responses[i] = p.runBatch(ctx, &b)
		wrote = wrote || (b.write != nil && out.Responses[i].Error == nil)

		if dup {
			if wrote {
				p.notifyChanged()
			}

			return nil, value.Invalid(msgBatchDuplicates)
		}
	}

	return out, nil
}

// parsedStatement is a statement of a batch, parsed or refused.
type parsedStatement struct {
	stmt *partiql.Statement
	err  error
}

// parseBatch parses a batch's statements and makes the checks on the whole
// batch: no empty statement, at most 25, reads and writes not mixed.
func parseBatch(reqs []api.BatchStatementRequest) ([]parsedStatement, error) {
	if len(reqs) == 0 {
		return nil, value.Invalid(msgStatementEmpty)
	}

	out := make([]parsedStatement, len(reqs))
	reads, writes := 0, 0

	for i := range reqs {
		if reqs[i].Statement == nil {
			return nil, value.Invalid(msgStatementEmpty)
		}

		stmt, err := parseStatement(reqs[i].Statement, reqs[i].Parameters, false)
		if err != nil && messageOf(err) == partiql.MsgVariable {
			// A batch names a lone name in its own words (verification/SQ-229
			// probe5).
			err = value.Invalid(msgBatchNotStatement)
		}

		out[i] = parsedStatement{stmt: stmt, err: err}

		switch {
		case err != nil:
		case stmt.Kind == partiql.Select:
			reads++
		default:
			writes++
		}
	}

	switch {
	case len(reqs) > maxBatchWrites && reads > 0:
		return nil, value.Invalid(msgBatchReads)
	case len(reqs) > maxBatchWrites:
		return nil, value.Invalid(msgBatchWrites)
	case reads > 0 && writes > 0:
		return nil, value.Invalid(msgBatchMixed)
	default:
		return out, nil
	}
}

// compileBatch compiles one parsed statement of a batch into b, and returns
// the item key it names and its table, nil when it failed.
func (c caller) compileBatch(
	b *batchStatement, stmt *partiql.Statement, req *api.BatchStatementRequest,
) (value.Item, string) {
	returnOld, err := returnOnConditionFailure(req.ReturnValuesOnConditionCheckFailure)
	if err != nil {
		b.err = err

		return nil, ""
	}

	if stmt.Kind == partiql.Select {
		b.read, b.err = c.compileRead(stmt, req.Parameters, req.ConsistentRead)
		if errors.Is(b.err, errNotSingle) {
			b.err = value.Invalid(msgSingleSelect)
		}

		if b.err != nil {
			return nil, ""
		}

		return b.read.key, b.read.table
	}

	b.write, b.err = c.compileWrite(stmt, req.Parameters, returnOld)
	if b.err != nil {
		return nil, ""
	}

	return b.write.key, b.write.table
}

// itemKeyString identifies the item key in table.
func itemKeyString(table string, key value.Item) string {
	var b strings.Builder

	b.WriteString(strconv.Quote(table))

	for name, v := range key {
		b.WriteString(" " + strconv.Quote(name) + "=" + value.KeyString(&v))
	}

	return b.String()
}

// runBatch runs one statement of a batch and answers it.
func (p *Provider) runBatch(ctx context.Context, b *batchStatement) api.BatchStatementResponse {
	switch {
	case b.err != nil:
		return batchError(b.err, "")
	case b.read != nil:
		out, err := p.GetItem(ctx, b.read.get)
		if err != nil {
			return batchError(err, b.read.table)
		}

		return api.BatchStatementResponse{TableName: &b.read.table, Item: shapeItem(out.Item, b.read.aliases)}
	default:
		item, _, err := p.runWrite(ctx, b.write, nil)
		if err != nil {
			return batchError(err, b.write.table)
		}

		return api.BatchStatementResponse{TableName: &b.write.table, Item: item}
	}
}

// batchError is a statement's error as BatchStatementError's codes name it.
// The table is named only when the statement failed on its item.
func batchError(err error, table string) api.BatchStatementResponse {
	code, msg := "InternalServerError", err.Error()

	if ae, ok := errors.AsType[*awserr.Error](statementFailure(err)); ok {
		code, msg = strings.TrimSuffix(ae.Code, "Exception"), ae.Message
	}

	if code == "Validation" {
		code = "ValidationError"
	}

	// A statement that failed on its item names its table: a failed
	// condition, a duplicate, or an update of a key attribute
	// (verification/SQ-229 probe5).
	out := api.BatchStatementResponse{Error: &api.BatchStatementError{Code: &code, Message: &msg, Item: failedItem(err)}}
	if table != "" {
		out.TableName = &table
	}

	return out
}

// notifyChanged tells persistence about a change the gateway does not see.
func (p *Provider) notifyChanged() {
	if p.changed != nil {
		p.changed(api.Metadata.Name)
	}
}
