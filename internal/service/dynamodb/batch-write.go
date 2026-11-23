package dynamodb

import (
	"context"
	"errors"
	"fmt"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// The messages DynamoDB Local answers for a BatchWriteItem it refuses, and the
// one LocalStack puts in place of its missing-key message.
const (
	msgBatchWriteTooMany = "Too many items requested for the BatchWriteItem call"
	msgBatchWriteNone    = "BatchWriteItem cannot have a null or no requests set"
	msgBatchWriteEmpty   = "The batch write request list for a table cannot be null or empty: "
	msgBatchWriteRequest = "Supplied AttributeValue has more than one datatypes set, " +
		"must contain exactly one of the supported datatypes"
	msgBatchWriteNull   = "The supplied Item contains a null AttributeValue"
	msgBatchDuplicates  = "Provided list of item keys contains duplicates"
	msgKeyMissing       = "One of the required keys was not given a value"
	msgKeySchemaMissing = "The provided key element does not match the schema"
)

// batchWrite is one checked put or delete of a BatchWriteItem.
type batchWrite struct {
	request api.WriteRequest
	// key locates the item; item is what is stored there, nil to delete.
	key  value.Item
	item value.Item
}

// batchTable is one table of a checked BatchWriteItem.
type batchTable struct {
	name   string
	entry  *tableEntry
	writes []batchWrite
}

// BatchWriteItem puts and deletes up to 25 items across tables, each through
// the one write path, with no conditions. A request LocalStack's
// DYNAMODB_*_ERROR_PROBABILITY throttles is returned in UnprocessedItems and
// not written.
func (p *Provider) BatchWriteItem(ctx context.Context, in *api.BatchWriteItemInput) (*api.BatchWriteItemOutput, error) {
	tables, err := p.caller(ctx).checkBatchWrite(in.RequestItems)
	if err != nil {
		// LocalStack answers the schema message where DynamoDB Local finds a
		// key attribute missing (batch_write_item in provider.py).
		var e *awserr.Error
		if errors.As(err, &e) && e.Message == msgKeyMissing {
			return nil, value.Invalid(msgKeySchemaMissing)
		}

		return nil, err
	}

	out := &api.BatchWriteItemOutput{UnprocessedItems: map[string][]api.WriteRequest{}}
	used := tableConsumption{}

	for _, t := range tables {
		for _, w := range t.writes {
			if p.throttled("BatchWriteItem") {
				out.UnprocessedItems[t.name] = append(out.UnprocessedItems[t.name], w.request)

				continue
			}

			change, err := t.entry.data.Write(w.key, func(value.Packed) (value.Item, error) { return w.item, nil })
			if err != nil {
				return nil, fmt.Errorf("batch write: %w", err)
			}

			used.on(t.name).add(writeCapacity(t.entry, change.OldPacked, change.New))
		}
	}

	out.ConsumedCapacity = used.report(in.ReturnConsumedCapacity)

	return out, nil
}

// checkBatchWrite checks a BatchWriteItem's requests, as DynamoDB Local orders
// its checks: the count, then table by table in its order, each request in
// turn.
func (c caller) checkBatchWrite(requests map[string][]api.WriteRequest) ([]batchTable, error) {
	count := 0
	for _, list := range requests {
		count += len(list)
	}

	if count > maxBatchWrites {
		return nil, value.Invalid(msgBatchWriteTooMany)
	}

	if len(requests) == 0 {
		return nil, value.Invalid(msgBatchWriteNone)
	}

	names := requestTables(requests)
	tables := make([]batchTable, 0, len(names))

	for _, name := range names {
		t, err := c.checkBatchTable(name, requests[name])
		if err != nil {
			return nil, err
		}

		tables = append(tables, t)
	}

	return tables, nil
}

func (c caller) checkBatchTable(name string, requests []api.WriteRequest) (batchTable, error) {
	name = tableName(name)

	err := checkName(&name)
	if err != nil {
		return batchTable{}, err
	}

	if len(requests) == 0 {
		return batchTable{}, value.Invalid(msgBatchWriteEmpty + name)
	}

	_, e, err := c.lookup(&name)
	if err != nil {
		return batchTable{}, err
	}

	t := batchTable{name: name, entry: e, writes: make([]batchWrite, 0, len(requests))}
	seen := make(map[string]struct{}, len(requests))

	for _, request := range requests {
		w, err := checkWriteRequest(e, request)
		if err != nil {
			return batchTable{}, err
		}

		id := itemKey(e, w.key)
		if _, dup := seen[id]; dup {
			return batchTable{}, value.Invalid(msgBatchDuplicates)
		}

		seen[id] = struct{}{}
		t.writes = append(t.writes, w)
	}

	return t, nil
}

// checkWriteRequest checks one PutRequest or DeleteRequest against the table.
func checkWriteRequest(e *tableEntry, request api.WriteRequest) (batchWrite, error) {
	put, del := request.PutRequest, request.DeleteRequest
	if (put == nil) == (del == nil) {
		return batchWrite{}, value.Invalid(msgBatchWriteRequest)
	}

	if put != nil {
		if put.Item == nil {
			return batchWrite{}, value.Invalid(msgBatchWriteNull)
		}

		err := value.ValidateItem(put.Item)
		if err == nil {
			err = e.data.CheckItem(put.Item)
		}

		if err != nil {
			return batchWrite{}, fmt.Errorf("batch put: %w", err)
		}

		return batchWrite{request: request, key: put.Item, item: put.Item}, nil
	}

	if del.Key == nil {
		return batchWrite{}, value.Invalid(msgBatchWriteNull)
	}

	err := value.ValidateItem(del.Key)
	if err == nil {
		err = e.data.CheckKey(del.Key)
	}

	if err != nil {
		return batchWrite{}, fmt.Errorf("batch delete: %w", err)
	}

	return batchWrite{request: request, key: del.Key}, nil
}
