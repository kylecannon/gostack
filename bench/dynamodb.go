package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"sync/atomic"
	"time"
)

// This file holds the DynamoDB workloads. Each speaks json 1.0 over plain HTTP
// with a SigV4 header the services route on and nobody checks, as the S3 and KMS
// workloads do, so the repository takes no dependency on the SDK's dynamodb
// module. A cell makes its table, and the items its reads need, before the clock
// starts. Every worker writes and reads keys of its own partition, w<n>, so no
// two workers contend for an item and a transaction never conflicts.
//
//	dynamodb/item      PutItem, then GetItem of the item it wrote, at 100 B,
//	                   4 KiB and 399 KiB
//	dynamodb/query     Query of the worker's partition of ten 100 B items
//	dynamodb/update    UpdateItem of SET n = n + :one under attribute_exists(pk)
//	dynamodb/transact  TransactWriteItems of two 100 B Puts
//	dynamodb/stream    GetRecords of a shard holding 100 records of 100 B items,
//	                   from an iterator the worker took before the clock
//
// Each write replaces an item the cell already holds, or adds to a number, so a
// persistence-on cell saves a state of the same size every time.

const (
	serviceDynamoDB    = "dynamodb"
	persistEnvDynamoDB = "PERSIST_DYNAMODB"

	workloadDDBItem     = "item"
	workloadDDBQuery    = "query"
	workloadDDBUpdate   = "update"
	workloadDDBTransact = "transact"
	workloadDDBStream   = "stream"

	opDDBPut        = "put"
	opDDBGet        = "get"
	opDDBQuery      = "query"
	opDDBUpdate     = "update"
	opDDBTransact   = "transact"
	opDDBGetRecords = "getrecords"

	ddbPayloadSmall  = 100
	ddbPayloadMedium = 4 * bytesPerKiB
	// ddbPayloadLarge is the largest whole KiB whose item, with its key and
	// attribute names, stays inside DynamoDB's 400 KB item limit.
	ddbPayloadLarge = 399 * bytesPerKiB

	// ddbQueryItems is how many items a worker's partition holds for query.
	ddbQueryItems = 10
	// ddbStreamRecords is how many records the stream cell's shard holds.
	ddbStreamRecords = 100
	// ddbStreamWait bounds how long a stream cell waits, before the clock, for
	// every record it wrote to be readable. LocalStack feeds its streams through
	// Kinesis, which takes a moment after CreateTable.
	ddbStreamWait     = 60 * time.Second
	ddbStreamInterval = 250 * time.Millisecond

	// ddbAuth is a SigV4 header that names the dynamodb signing name, which DynamoDB
	// Streams signs with too, and a region. The signature is not checked.
	ddbAuth = "AWS4-HMAC-SHA256 Credential=test/20260101/us-east-1/dynamodb/aws4_request, " +
		"SignedHeaders=host;x-amz-date, Signature=abc"
	ddbContentType   = "application/x-amz-json-1.0"
	ddbTarget        = "DynamoDB_20120810."
	ddbStreamsTarget = "DynamoDBStreams_20120810."

	ddbTableName     = "TableName"
	ddbItemMember    = "Item"
	ddbAttributeName = "AttributeName"
	ddbAttributeType = "AttributeType"
	ddbKeySchema     = "KeySchema"
	ddbKeyType       = "KeyType"
)

// ddbLevels are the concurrencies of every DynamoDB cell.
var ddbLevels = []int{levelLow, levelMid, levelHigh}

// ddbPersistence are the persistence settings of the DynamoDB cells, "" being off.
var ddbPersistence = []string{"", persistEnvDynamoDB}

var errDynamoDB = errors.New("dynamodb")

// ddbTables numbers the tables this process makes, so cells that share one
// server, as the in-process test does, never share a table.
var ddbTables atomic.Int64

// ddbClient sends DynamoDB and DynamoDB Streams requests to one endpoint.
type ddbClient struct {
	http     *http.Client
	endpoint string
}

func newDDBClient(endpoint string, workers int) ddbClient {
	return ddbClient{http: &http.Client{Transport: newTransport(workers)}, endpoint: endpoint}
}

// send posts body to operation under target and decodes the answer into out,
// which may be nil.
func (c ddbClient) send(ctx context.Context, target, operation string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build %s: %w", operation, err)
	}

	req.Header.Set("Authorization", ddbAuth)
	req.Header.Set("Content-Type", ddbContentType)
	req.Header.Set("X-Amz-Date", s3Date)
	req.Header.Set("X-Amz-Target", target+operation)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}

	defer func() { _ = resp.Body.Close() }()

	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s: read the answer: %w", operation, err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s: status %d: %.*s", errDynamoDB, operation, resp.StatusCode, formErrorHead, answer)
	}

	if out == nil {
		return nil
	}

	err = json.Unmarshal(answer, out)
	if err != nil {
		return fmt.Errorf("%s: decode the answer: %w", operation, err)
	}

	return nil
}

// call encodes input and sends it to a DynamoDB operation.
func (c ddbClient) call(ctx context.Context, operation string, input, out any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("%s: encode the request: %w", operation, err)
	}

	return c.send(ctx, ddbTarget, operation, body, out)
}

// streams encodes input and sends it to a DynamoDB Streams operation.
func (c ddbClient) streams(ctx context.Context, operation string, input, out any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("%s: encode the request: %w", operation, err)
	}

	return c.send(ctx, ddbStreamsTarget, operation, body, out)
}

// ddbS is a string attribute value.
type ddbS struct {
	S string `json:"S"`
}

// ddbTableDescription is the part of CreateTable's answer a cell reads.
type ddbTableDescription struct {
	LatestStreamArn string
}

// ddbShard is the part of a shard's description a cell reads.
type ddbShard struct {
	ShardID string `json:"ShardId"`
}

// ddbStreamDescription is the part of DescribeStream's answer a cell reads.
type ddbStreamDescription struct {
	Shards []ddbShard
}

// ddbN is a number attribute value.
type ddbN struct {
	N string `json:"N"`
}

// createTable makes a table keyed on pk (hash) and sk (range), both strings,
// with its stream on when viewType is not "", and returns its name and the ARN
// of its stream.
func (c ddbClient) createTable(ctx context.Context, viewType string) (name, streamARN string, err error) {
	name = "bench-" + strconv.FormatInt(ddbTables.Add(1), 10)
	input := map[string]any{
		ddbTableName: name,
		ddbKeySchema: []map[string]string{
			{ddbAttributeName: "pk", ddbKeyType: "HASH"},
			{ddbAttributeName: "sk", ddbKeyType: "RANGE"},
		},
		"AttributeDefinitions": []map[string]string{
			{ddbAttributeName: "pk", ddbAttributeType: "S"},
			{ddbAttributeName: "sk", ddbAttributeType: "S"},
		},
		"BillingMode": "PAY_PER_REQUEST",
	}

	if viewType != "" {
		input["StreamSpecification"] = map[string]any{"StreamEnabled": true, "StreamViewType": viewType}
	}

	var out struct{ TableDescription ddbTableDescription }

	err = c.call(ctx, "CreateTable", input, &out)
	if err != nil {
		return "", "", err
	}

	if viewType != "" && out.TableDescription.LatestStreamArn == "" {
		return "", "", fmt.Errorf("%w: CreateTable answered no LatestStreamArn", errDynamoDB)
	}

	return name, out.TableDescription.LatestStreamArn, nil
}

// putBody is the PutItem request of the item pk, sk with a payload-byte string v.
func putBody(table, pk, sk string, payload int) ([]byte, error) {
	body, err := json.Marshal(map[string]any{ddbTableName: table, ddbItemMember: ddbItem(pk, sk, payload)})
	if err != nil {
		return nil, fmt.Errorf("encode PutItem: %w", err)
	}

	return body, nil
}

func ddbItem(pk, sk string, payload int) map[string]any {
	return map[string]any{"pk": ddbS{pk}, "sk": ddbS{sk}, "v": ddbS{payloadBody(payload)}}
}

func ddbKey(pk, sk string) map[string]ddbS {
	return map[string]ddbS{"pk": {pk}, "sk": {sk}}
}

func workerKey(worker int) string { return "w" + strconv.Itoa(worker) }

func ddbCell(workload string, payload int, persistEnv string, concurrency int) cellSpec {
	return cellSpec{
		Service: serviceDynamoDB, Workload: workload, Payload: payload,
		PersistEnv: persistEnv, Concurrency: concurrency,
	}
}

// ddbLevelCells is a workload's cells at one payload: every concurrency,
// persistence off and on.
func ddbLevelCells(workload string, payload int) []cellSpec {
	cells := make([]cellSpec, 0, len(ddbPersistence)*len(ddbLevels))

	for _, persistEnv := range ddbPersistence {
		for _, level := range ddbLevels {
			cells = append(cells, ddbCell(workload, payload, persistEnv, level))
		}
	}

	return cells
}

// checkDDBPayload refuses a cell whose payload is not one the workload runs.
func checkDDBPayload(cell cellSpec, sizes ...int) error {
	err := checkLevels(cell)
	if err != nil {
		return err
	}

	if slices.Contains(sizes, cell.Payload) {
		return nil
	}

	return fmt.Errorf("%w: payload %d, want one of %v", errBadAxis, cell.Payload, sizes)
}

// itemWorkload is PutItem, then GetItem of the item it wrote.
type itemWorkload struct{}

func (itemWorkload) Service() string { return serviceDynamoDB }
func (itemWorkload) Name() string    { return workloadDDBItem }
func (itemWorkload) Ops() []string   { return []string{opDDBPut, opDDBGet} }

func (itemWorkload) Run(ctx context.Context, c client, cell cellSpec, duration time.Duration) (runResult, error) {
	err := checkDDBPayload(cell, ddbPayloadSmall, ddbPayloadMedium, ddbPayloadLarge)
	if err != nil {
		return runResult{}, err
	}

	dc := newDDBClient(c.endpoint, cell.Concurrency)

	table, _, err := dc.createTable(ctx, "")
	if err != nil {
		return runResult{}, err
	}

	puts := make([][]byte, cell.Concurrency)
	gets := make([][]byte, cell.Concurrency)

	for i := range cell.Concurrency {
		puts[i], err = putBody(table, workerKey(i), "item", cell.Payload)
		if err != nil {
			return runResult{}, err
		}

		gets[i], err = json.Marshal(map[string]any{ddbTableName: table, "Key": ddbKey(workerKey(i), "item")})
		if err != nil {
			return runResult{}, fmt.Errorf("encode GetItem: %w", err)
		}
	}

	l := loop{workers: cell.Concurrency, duration: duration, ops: itemWorkload{}.Ops()}

	return l.run(ctx, func(ctx context.Context, worker int, rec *recorder) {
		if !rec.measure(opDDBPut, func() error {
			return dc.send(ctx, ddbTarget, "PutItem", puts[worker], nil)
		}) {
			return
		}

		if rec.measure(opDDBGet, func() error {
			var out struct{ Item map[string]ddbS }

			getErr := dc.send(ctx, ddbTarget, "GetItem", gets[worker], &out)
			if getErr != nil {
				return getErr
			}

			if got := len(out.Item["v"].S); got != cell.Payload {
				return fmt.Errorf("%w: GetItem answered a %d-byte v, not the %d put", errDynamoDB, got, cell.Payload)
			}

			return nil
		}) {
			rec.cycles++
		}
	})
}

// Cells is each payload at every concurrency, persistence off and on. The quick
// profile is 100 B at concurrency 16, persistence off and on.
func (itemWorkload) Cells(profile string) []cellSpec {
	if profile == profileQuick {
		return []cellSpec{
			ddbCell(workloadDDBItem, ddbPayloadSmall, "", levelMid),
			ddbCell(workloadDDBItem, ddbPayloadSmall, persistEnvDynamoDB, levelMid),
		}
	}

	var cells []cellSpec

	for _, payload := range []int{ddbPayloadSmall, ddbPayloadMedium, ddbPayloadLarge} {
		cells = append(cells, ddbLevelCells(workloadDDBItem, payload)...)
	}

	return cells
}

// queryWorkload is a Query of the worker's partition.
type queryWorkload struct{}

func (queryWorkload) Service() string { return serviceDynamoDB }
func (queryWorkload) Name() string    { return workloadDDBQuery }
func (queryWorkload) Ops() []string   { return []string{opDDBQuery} }

func (queryWorkload) Run(ctx context.Context, c client, cell cellSpec, duration time.Duration) (runResult, error) {
	err := checkDDBPayload(cell, ddbPayloadSmall)
	if err != nil {
		return runResult{}, err
	}

	dc := newDDBClient(c.endpoint, cell.Concurrency)

	table, _, err := dc.createTable(ctx, "")
	if err != nil {
		return runResult{}, err
	}

	queries := make([][]byte, cell.Concurrency)

	for i := range cell.Concurrency {
		for n := range ddbQueryItems {
			put, putErr := putBody(table, workerKey(i), strconv.Itoa(n), cell.Payload)
			if putErr != nil {
				return runResult{}, putErr
			}

			putErr = dc.send(ctx, ddbTarget, "PutItem", put, nil)
			if putErr != nil {
				return runResult{}, putErr
			}
		}

		queries[i], err = json.Marshal(map[string]any{
			ddbTableName:                table,
			"KeyConditionExpression":    "pk = :pk",
			"ExpressionAttributeValues": map[string]ddbS{":pk": {workerKey(i)}},
		})
		if err != nil {
			return runResult{}, fmt.Errorf("encode Query: %w", err)
		}
	}

	l := loop{workers: cell.Concurrency, duration: duration, ops: queryWorkload{}.Ops()}

	return l.run(ctx, func(ctx context.Context, worker int, rec *recorder) {
		if rec.measure(opDDBQuery, func() error {
			var out struct{ Count int }

			queryErr := dc.send(ctx, ddbTarget, "Query", queries[worker], &out)
			if queryErr != nil {
				return queryErr
			}

			if out.Count != ddbQueryItems {
				return fmt.Errorf("%w: Query counted %d items, not %d", errDynamoDB, out.Count, ddbQueryItems)
			}

			return nil
		}) {
			rec.cycles++
		}
	})
}

// Cells is every concurrency, persistence off and on, at 100 B items. The quick
// profile is concurrency 16.
func (queryWorkload) Cells(profile string) []cellSpec {
	if profile == profileQuick {
		return []cellSpec{ddbCell(workloadDDBQuery, ddbPayloadSmall, "", levelMid)}
	}

	return ddbLevelCells(workloadDDBQuery, ddbPayloadSmall)
}

// updateWorkload is a conditional UpdateItem that adds one to a number.
type updateWorkload struct{}

func (updateWorkload) Service() string { return serviceDynamoDB }
func (updateWorkload) Name() string    { return workloadDDBUpdate }
func (updateWorkload) Ops() []string   { return []string{opDDBUpdate} }

func (updateWorkload) Run(ctx context.Context, c client, cell cellSpec, duration time.Duration) (runResult, error) {
	err := checkDDBPayload(cell, ddbPayloadSmall)
	if err != nil {
		return runResult{}, err
	}

	dc := newDDBClient(c.endpoint, cell.Concurrency)

	table, _, err := dc.createTable(ctx, "")
	if err != nil {
		return runResult{}, err
	}

	updates := make([][]byte, cell.Concurrency)

	for i := range cell.Concurrency {
		item := ddbItem(workerKey(i), "counter", cell.Payload)
		item["n"] = ddbN{"0"}

		err = dc.call(ctx, "PutItem", map[string]any{ddbTableName: table, ddbItemMember: item}, nil)
		if err != nil {
			return runResult{}, err
		}

		updates[i], err = json.Marshal(map[string]any{
			ddbTableName:                table,
			"Key":                       ddbKey(workerKey(i), "counter"),
			"UpdateExpression":          "SET n = n + :one",
			"ConditionExpression":       "attribute_exists(pk)",
			"ExpressionAttributeValues": map[string]ddbN{":one": {"1"}},
		})
		if err != nil {
			return runResult{}, fmt.Errorf("encode UpdateItem: %w", err)
		}
	}

	l := loop{workers: cell.Concurrency, duration: duration, ops: updateWorkload{}.Ops()}

	return l.run(ctx, func(ctx context.Context, worker int, rec *recorder) {
		if rec.measure(opDDBUpdate, func() error {
			return dc.send(ctx, ddbTarget, "UpdateItem", updates[worker], nil)
		}) {
			rec.cycles++
		}
	})
}

// Cells is every concurrency, persistence off and on, of an item holding 100 B
// besides its number. The quick profile is concurrency 16.
func (updateWorkload) Cells(profile string) []cellSpec {
	if profile == profileQuick {
		return []cellSpec{ddbCell(workloadDDBUpdate, ddbPayloadSmall, "", levelMid)}
	}

	return ddbLevelCells(workloadDDBUpdate, ddbPayloadSmall)
}

// transactWorkload is a TransactWriteItems of two Puts in the worker's
// partition.
type transactWorkload struct{}

func (transactWorkload) Service() string { return serviceDynamoDB }
func (transactWorkload) Name() string    { return workloadDDBTransact }
func (transactWorkload) Ops() []string   { return []string{opDDBTransact} }

func (transactWorkload) Run(ctx context.Context, c client, cell cellSpec, duration time.Duration) (runResult, error) {
	err := checkDDBPayload(cell, ddbPayloadSmall)
	if err != nil {
		return runResult{}, err
	}

	dc := newDDBClient(c.endpoint, cell.Concurrency)

	table, _, err := dc.createTable(ctx, "")
	if err != nil {
		return runResult{}, err
	}

	transactions := make([][]byte, cell.Concurrency)

	for i := range cell.Concurrency {
		transactions[i], err = json.Marshal(map[string]any{
			"TransactItems": []map[string]any{
				{"Put": map[string]any{ddbTableName: table, ddbItemMember: ddbItem(workerKey(i), "a", cell.Payload)}},
				{"Put": map[string]any{ddbTableName: table, ddbItemMember: ddbItem(workerKey(i), "b", cell.Payload)}},
			},
		})
		if err != nil {
			return runResult{}, fmt.Errorf("encode TransactWriteItems: %w", err)
		}
	}

	l := loop{workers: cell.Concurrency, duration: duration, ops: transactWorkload{}.Ops()}

	return l.run(ctx, func(ctx context.Context, worker int, rec *recorder) {
		if rec.measure(opDDBTransact, func() error {
			return dc.send(ctx, ddbTarget, "TransactWriteItems", transactions[worker], nil)
		}) {
			rec.cycles++
		}
	})
}

// Cells is every concurrency, persistence off and on, at 100 B items. The quick
// profile is concurrency 16.
func (transactWorkload) Cells(profile string) []cellSpec {
	if profile == profileQuick {
		return []cellSpec{ddbCell(workloadDDBTransact, ddbPayloadSmall, "", levelMid)}
	}

	return ddbLevelCells(workloadDDBTransact, ddbPayloadSmall)
}

// streamWorkload is GetRecords of one shard. Nothing is written once the
// clock starts, so the shard holds the same records throughout.
type streamWorkload struct{}

func (streamWorkload) Service() string { return serviceDynamoDB }
func (streamWorkload) Name() string    { return workloadDDBStream }
func (streamWorkload) Ops() []string   { return []string{opDDBGetRecords} }

func (streamWorkload) Run(ctx context.Context, c client, cell cellSpec, duration time.Duration) (runResult, error) {
	err := checkDDBPayload(cell, ddbPayloadSmall)
	if err != nil {
		return runResult{}, err
	}

	dc := newDDBClient(c.endpoint, cell.Concurrency)

	streamARN, shard, err := dc.seedStream(ctx, cell.Payload)
	if err != nil {
		return runResult{}, err
	}

	reads := make([][]byte, cell.Concurrency)

	for i := range cell.Concurrency {
		iterator, iterErr := dc.iterator(ctx, streamARN, shard)
		if iterErr != nil {
			return runResult{}, iterErr
		}

		reads[i], err = json.Marshal(map[string]any{"ShardIterator": iterator, "Limit": ddbStreamRecords})
		if err != nil {
			return runResult{}, fmt.Errorf("encode GetRecords: %w", err)
		}
	}

	l := loop{workers: cell.Concurrency, duration: duration, ops: streamWorkload{}.Ops()}

	return l.run(ctx, func(ctx context.Context, worker int, rec *recorder) {
		if rec.measure(opDDBGetRecords, func() error {
			var out struct{ Records []json.RawMessage }

			readErr := dc.send(ctx, ddbStreamsTarget, "GetRecords", reads[worker], &out)
			if readErr != nil {
				return readErr
			}

			if len(out.Records) == 0 {
				return fmt.Errorf("%w: GetRecords answered no record", errDynamoDB)
			}

			return nil
		}) {
			rec.cycles++
		}
	})
}

// seedStream makes a table with a NEW_IMAGE stream, puts ddbStreamRecords
// items of payload bytes, waits until the stream holds all of their records,
// and returns the stream's ARN and its shard's id.
func (c ddbClient) seedStream(ctx context.Context, payload int) (streamARN, shard string, err error) {
	table, streamARN, err := c.createTable(ctx, "NEW_IMAGE")
	if err != nil {
		return "", "", err
	}

	for n := range ddbStreamRecords {
		put, putErr := putBody(table, "stream", strconv.Itoa(n), payload)
		if putErr != nil {
			return "", "", putErr
		}

		putErr = c.send(ctx, ddbTarget, "PutItem", put, nil)
		if putErr != nil {
			return "", "", putErr
		}
	}

	shard, err = c.awaitRecords(ctx, streamARN)
	if err != nil {
		return "", "", err
	}

	return streamARN, shard, nil
}

// iterator returns a TRIM_HORIZON iterator of the shard.
func (c ddbClient) iterator(ctx context.Context, streamARN, shard string) (string, error) {
	var out struct{ ShardIterator string }

	err := c.streams(ctx, "GetShardIterator", map[string]string{
		"StreamArn": streamARN, "ShardId": shard, "ShardIteratorType": "TRIM_HORIZON",
	}, &out)
	if err != nil {
		return "", err
	}

	if out.ShardIterator == "" {
		return "", fmt.Errorf("%w: GetShardIterator answered no ShardIterator", errDynamoDB)
	}

	return out.ShardIterator, nil
}

// awaitRecords waits until the stream's first shard holds every record the cell
// wrote, and returns the shard's id.
func (c ddbClient) awaitRecords(ctx context.Context, streamARN string) (string, error) {
	deadline := time.Now().Add(ddbStreamWait)

	for {
		shard, count, err := c.countRecords(ctx, streamARN)
		if err == nil && count >= ddbStreamRecords {
			return shard, nil
		}

		if time.Now().After(deadline) {
			if err == nil {
				err = fmt.Errorf("%w: the stream holds %d of %d records", errDynamoDB, count, ddbStreamRecords)
			}

			return "", fmt.Errorf("after %s: %w", ddbStreamWait, err)
		}

		select {
		case <-ctx.Done():
			return "", fmt.Errorf("wait for the stream: %w", ctx.Err())
		case <-time.After(ddbStreamInterval):
		}
	}
}

// countRecords reads the stream's first shard from its start, one page, and
// returns its id and how many records the page held.
func (c ddbClient) countRecords(ctx context.Context, streamARN string) (string, int, error) {
	var described struct{ StreamDescription ddbStreamDescription }

	err := c.streams(ctx, "DescribeStream", map[string]string{"StreamArn": streamARN}, &described)
	if err != nil {
		return "", 0, err
	}

	if len(described.StreamDescription.Shards) == 0 {
		return "", 0, fmt.Errorf("%w: the stream has no shard", errDynamoDB)
	}

	shard := described.StreamDescription.Shards[0].ShardID

	iterator, err := c.iterator(ctx, streamARN, shard)
	if err != nil {
		return "", 0, err
	}

	var records struct{ Records []json.RawMessage }

	err = c.streams(ctx, "GetRecords", map[string]any{"ShardIterator": iterator, "Limit": ddbStreamRecords}, &records)
	if err != nil {
		return "", 0, err
	}

	return shard, len(records.Records), nil
}

// Cells is every concurrency, persistence off and on, at 100 B items. The quick
// profile is concurrency 16.
func (streamWorkload) Cells(profile string) []cellSpec {
	if profile == profileQuick {
		return []cellSpec{ddbCell(workloadDDBStream, ddbPayloadSmall, "", levelMid)}
	}

	return ddbLevelCells(workloadDDBStream, ddbPayloadSmall)
}
