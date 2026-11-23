package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// This file holds the large-table DynamoDB workload, which only -profile large
// runs (docs/standards.md, "System bench"). One cell is a container with a memory
// limit, one table with a global secondary index, items of a realistic mix put in
// it before the clock starts, and then a mixed load for a fixed time:
//
//	items       25k, 50k, 100k, 300k and 1M, so a heavier representation reaches
//	            the limit at one of them
//	persistence off and PERSIST_DYNAMODB=1
//	concurrency 16 and 64
//	memory      1g, or -memory
//
// An item has 10 to 30 attributes (strings, numbers, a boolean, small maps and
// lists) and a body of 100 B to 4 KiB, its size spread evenly on a log scale.
// The text is random, so a representation that compresses runs of one byte gains
// nothing here. Every byte of an item follows from its index and a version, so
// two images are loaded with the same table.
//
// The load is 70% reads and 30% writes, one operation a cycle:
//
//	get         35%  GetItem
//	query       20%  Query of a partition (20 items) or of the index, a third of
//	                 them with a FilterExpression
//	scan        15%  a Scan page of 25 items from a random key, a third of them
//	                 with a FilterExpression
//	put         10%  PutItem over an existing key
//	update      10%  UpdateItem: SET a number, ADD one to another
//	delete       5%  DeleteItem
//	batchwrite   5%  BatchWriteItem of 25 puts over existing keys
//
// Writes pick keys anywhere in the table and a put or an update makes an item
// that a delete took, so the table stays within a few percent of its size.

const (
	workloadDDBLarge = "large"
	optItems         = "items"

	opDDBScan       = "scan"
	opDDBDelete     = "delete"
	opDDBBatchWrite = "batchwrite"

	// ddbLargeMemory is the memory limit of a large cell, until -memory replaces it.
	ddbLargeMemory = "1g"

	ddbLargeIndex      = "gsi1"
	ddbLargePartition  = 20
	ddbLargeCategories = 1000
	ddbLargeKinds      = 8
	ddbBatchSize       = 25
	ddbSeedWorkers     = 16
	ddbQueryLimit      = 10
	ddbScanLimit       = 25
	ddbFilterEvery     = 3
	// ddbSeedFloor is the rate, in items a second, the fill of a cell is allowed to
	// fall to before its timeout ends it. A fill that slow is a failed run.
	ddbSeedFloor = 500
	// ddbSeedTimeoutExtra covers the fill's fixed costs.
	ddbSeedTimeoutExtra = time.Minute

	ddbMinAttributes = 10
	ddbMaxAttributes = 30
	// ddbNamedAttributes are the attributes every item has: the four keys, kind,
	// n and body.
	ddbNamedAttributes = 7
	ddbShapes          = 4
	ddbTextWord        = 10
	ddbDecimal         = 10
	ddbAlphabetBits    = 6
	ddbSeedStep        = 0x9e3779b97f4a7c15
	ddbIndexStride     = 7919
	ddbIndexModulus    = 1_000_000_007
	ddbKeyDigits       = 8
	ddbIndexDigits     = 10
	ddbNumberRange     = 1_000_000
	ddbShortMin        = 10
	ddbShortSpread     = 50
	ddbSmallMin        = 8
	ddbSmallSpread     = 16
)

// The mix, as the upper bound of each operation's share of ddbPercent.
const (
	ddbPercent = 100
	mixGet     = 35
	mixQuery   = 55
	mixScan    = 70
	mixPut     = 80
	mixUpdate  = 90
	mixDelete  = 95
)

// The shapes of the attributes past the named ones, in turn; the fourth is a list.
const (
	shapeString = iota
	shapeNumber
	shapeMap
)

const ddbAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

// ddbLargeSteps are the sizes of the tables a large run fills, in items. Under 1g
// the first holds in any image, a table of 100k items is past the limit of an
// image that holds an item as a decoded document (about 16 KiB each, SQ-522's
// first reading), and 1M is past it for a packed one (about 4.5 KiB each).
var ddbLargeSteps = []int{25_000, 50_000, 100_000, 300_000, 1_000_000}

// ddbLargeLevels are the concurrencies of a large cell.
var ddbLargeLevels = []int{levelMid, levelHigh}

var errLargeTable = errors.New("large table")

// largeTableWorkload is the mixed load over a table that fills a limited
// container.
type largeTableWorkload struct{}

func (largeTableWorkload) Service() string { return serviceDynamoDB }
func (largeTableWorkload) Name() string    { return workloadDDBLarge }

func (largeTableWorkload) Ops() []string {
	return []string{opDDBGet, opDDBQuery, opDDBScan, opDDBPut, opDDBUpdate, opDDBDelete, opDDBBatchWrite}
}

// Cells is every size at both concurrencies, persistence off and on. cellsFor
// asks for them in the large profile alone.
func (largeTableWorkload) Cells(string) []cellSpec {
	cells := make([]cellSpec, 0, len(ddbPersistence)*len(ddbLargeSteps)*len(ddbLargeLevels))

	for _, persistEnv := range ddbPersistence {
		for _, items := range ddbLargeSteps {
			for _, level := range ddbLargeLevels {
				cells = append(cells, ddbLargeCell(persistEnv, items, level))
			}
		}
	}

	return cells
}

func ddbLargeCell(persistEnv string, items, concurrency int) cellSpec {
	cell := ddbCell(workloadDDBLarge, 0, persistEnv, concurrency)
	cell.Options = []option{{Key: optItems, Value: itemsName(items)}}
	cell.Memory = ddbLargeMemory

	return cell
}

// SeedTime is how long the fill of the cell may take.
func (largeTableWorkload) SeedTime(cell cellSpec) time.Duration {
	items, err := cell.items()
	if err != nil {
		return ddbSeedTimeoutExtra
	}

	return time.Duration(items)*time.Second/ddbSeedFloor + ddbSeedTimeoutExtra
}

// itemsName writes a table size the way the cell id carries it: 100k, 1m.
func itemsName(items int) string {
	const (
		thousand = 1000
		million  = thousand * thousand
	)

	switch {
	case items >= million && items%million == 0:
		return strconv.Itoa(items/million) + "m"
	case items >= thousand && items%thousand == 0:
		return strconv.Itoa(items/thousand) + "k"
	default:
		return strconv.Itoa(items)
	}
}

// items is the size of the table the cell fills, from its items option.
func (c cellSpec) items() (int, error) {
	text := c.option(optItems)
	scale := 1

	switch {
	case strings.HasSuffix(text, "m"):
		text, scale = strings.TrimSuffix(text, "m"), 1_000_000
	case strings.HasSuffix(text, "k"):
		text, scale = strings.TrimSuffix(text, "k"), 1000
	default:
	}

	count, err := strconv.Atoi(text)
	if err != nil || count < 1 {
		return 0, fmt.Errorf("%w: %s=%q, want a size such as 100k", errBadAxis, optItems, c.option(optItems))
	}

	return count * scale, nil
}

func (w largeTableWorkload) Run(
	ctx context.Context, c client, cell cellSpec, duration time.Duration,
) (runResult, error) {
	err := checkLevels(cell)
	if err != nil {
		return runResult{}, err
	}

	items, err := cell.items()
	if err != nil {
		return runResult{}, err
	}

	dc := newDDBClient(c.endpoint, max(cell.Concurrency, ddbSeedWorkers))

	table, err := dc.createLargeTable(ctx)
	if err != nil {
		return runResult{}, err
	}

	filled, err := dc.fillLargeTable(ctx, table, items)
	if err != nil {
		return runResult{ItemsFilled: filled}, err
	}

	mixers := make([]*mixer, cell.Concurrency)
	for worker := range mixers {
		mixers[worker] = &mixer{dc: dc, table: table, items: items, rng: workerRand(worker)}
	}

	l := loop{workers: cell.Concurrency, duration: duration, ops: w.Ops()}

	run, err := l.run(ctx, func(ctx context.Context, worker int, rec *recorder) {
		mixers[worker].cycle(ctx, rec)
	})
	run.ItemsFilled = filled

	return run, err
}

// largeOnly makes the workload a largeWorkload.
func (largeTableWorkload) largeOnly() {}

// createLargeTable makes the table of a large cell: keyed on pk and sk, with a
// global secondary index on gsi1pk and gsi1sk that projects kind and n.
func (c ddbClient) createLargeTable(ctx context.Context) (string, error) {
	name := "bench-large-" + strconv.FormatInt(ddbTables.Add(1), 10)
	attribute := func(name string) map[string]string {
		return map[string]string{ddbAttributeName: name, ddbAttributeType: "S"}
	}
	keys := func(hash, rng string) []map[string]string {
		return []map[string]string{
			{ddbAttributeName: hash, ddbKeyType: "HASH"},
			{ddbAttributeName: rng, ddbKeyType: "RANGE"},
		}
	}
	attributes := []map[string]string{attribute("pk"), attribute("sk"), attribute("gsi1pk"), attribute("gsi1sk")}

	input := map[string]any{
		ddbTableName:           name,
		ddbKeySchema:           keys("pk", "sk"),
		"AttributeDefinitions": attributes,
		"BillingMode":          "PAY_PER_REQUEST",
		"GlobalSecondaryIndexes": []map[string]any{{
			"IndexName":  ddbLargeIndex,
			ddbKeySchema: keys("gsi1pk", "gsi1sk"),
			"Projection": map[string]any{
				"ProjectionType":   "INCLUDE",
				"NonKeyAttributes": []string{"kind", "n"},
			},
		}},
	}

	err := c.call(ctx, "CreateTable", input, nil)
	if err != nil {
		return "", err
	}

	return name, nil
}

// fillLargeTable puts items 0 to items-1 with BatchWriteItem on ddbSeedWorkers
// workers, then reads back the last one. It returns the items the server
// acknowledged, so a run that a container's death ended says how far the fill got.
func (c ddbClient) fillLargeTable(ctx context.Context, table string, items int) (int, error) {
	batches := (items + ddbBatchSize - 1) / ddbBatchSize

	var (
		next, accepted atomic.Int64
		once           sync.Once
		failure        error
		wg             sync.WaitGroup
	)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	for range ddbSeedWorkers {
		wg.Go(func() {
			var buf []byte

			for ctx.Err() == nil {
				batch := int(next.Add(1)) - 1
				if batch >= batches {
					return
				}

				first := batch * ddbBatchSize
				last := min(first+ddbBatchSize, items)
				buf = appendBatch(buf[:0], table, first, last, 0)

				err := c.writeBatch(ctx, buf)
				if err != nil {
					once.Do(func() { failure = err; cancel() })

					return
				}

				accepted.Add(int64(last - first))
			}
		})
	}

	wg.Wait()

	filled := int(accepted.Load())

	if failure != nil {
		return filled, fmt.Errorf("seeded %d of %d items: %w", filled, items, failure)
	}

	if ctx.Err() != nil {
		return filled, fmt.Errorf("seeded %d of %d items: %w", filled, items, ctx.Err())
	}

	return filled, c.checkLastItem(ctx, table, items)
}

// writeBatch sends a BatchWriteItem and fails when the server left an item
// unprocessed, which gostack never does: an item dropped there would make the
// table smaller than its cell says.
func (c ddbClient) writeBatch(ctx context.Context, body []byte) error {
	var out struct{ UnprocessedItems map[string][]any }

	err := c.send(ctx, ddbTarget, "BatchWriteItem", body, &out)
	if err != nil {
		return err
	}

	if len(out.UnprocessedItems) > 0 {
		return fmt.Errorf("%w: BatchWriteItem left items unprocessed", errLargeTable)
	}

	return nil
}

// checkLastItem reads back the last item the fill put, so a fill that put
// nothing is not mistaken for a table.
func (c ddbClient) checkLastItem(ctx context.Context, table string, items int) error {
	var out struct{ Item map[string]any }

	body := fmt.Appendf(nil, `{"TableName":%q,"Key":%s}`, table, keyJSON(items-1))

	err := c.send(ctx, ddbTarget, "GetItem", body, &out)
	if err != nil {
		return err
	}

	if len(out.Item) == 0 {
		return fmt.Errorf("%w: the fill's last item %d is not in the table", errLargeTable, items-1)
	}

	return nil
}

// mixer is one worker's load: the table, its size and the worker's own source of
// numbers. A cycle is one operation of the mix.
type mixer struct {
	dc    ddbClient
	table string
	items int
	rng   *rand.Rand
	buf   []byte
}

// workerRand is the source of a worker's choices. The seeds are fixed, so two
// runs of one cell make the same choices.
func workerRand(worker int) *rand.Rand {
	//nolint:gosec // the load's shape, not a secret.
	return rand.New(rand.NewPCG(uint64(worker)+1, ddbSeedStep))
}

// cycle runs one operation of the mix, chosen by the worker's source.
func (m *mixer) cycle(ctx context.Context, rec *recorder) {
	pick := m.rng.IntN(ddbPercent)
	key := m.rng.IntN(m.items)

	switch {
	case pick < mixGet:
		m.send(ctx, rec, opDDBGet, "GetItem", m.keyed(key))
	case pick < mixQuery:
		m.send(ctx, rec, opDDBQuery, "Query", m.query(key))
	case pick < mixScan:
		m.send(ctx, rec, opDDBScan, "Scan", m.scan(key))
	case pick < mixPut:
		m.buf = appendPut(m.buf[:0], m.table, key, m.rng.Uint64())
		m.send(ctx, rec, opDDBPut, "PutItem", m.buf)
	case pick < mixUpdate:
		m.send(ctx, rec, opDDBUpdate, "UpdateItem", m.update(key))
	case pick < mixDelete:
		m.send(ctx, rec, opDDBDelete, "DeleteItem", m.keyed(key))
	default:
		first := max(min(key, m.items-ddbBatchSize), 0)
		m.buf = appendBatch(m.buf[:0], m.table, first, min(first+ddbBatchSize, m.items), m.rng.Uint64())

		if rec.measure(opDDBBatchWrite, func() error { return m.dc.writeBatch(ctx, m.buf) }) {
			rec.cycles++
		}
	}
}

// send times one request of the mix and counts the cycle when it succeeds.
func (m *mixer) send(ctx context.Context, rec *recorder, op, target string, body []byte) {
	if rec.measure(op, func() error { return m.dc.send(ctx, ddbTarget, target, body, nil) }) {
		rec.cycles++
	}
}

// keyed is a request that holds a table and a key, a GetItem or a DeleteItem.
func (m *mixer) keyed(key int) []byte {
	return fmt.Appendf(nil, `{"TableName":%q,"Key":%s}`, m.table, keyJSON(key))
}

// update adds one to the item's number and sets another.
func (m *mixer) update(key int) []byte {
	return fmt.Appendf(nil, `{"TableName":%q,"Key":%s,"UpdateExpression":"SET touched = :t ADD n :one",`+
		`"ExpressionAttributeValues":{":t":{"N":"%d"},":one":{"N":"1"}}}`,
		m.table, keyJSON(key), m.rng.IntN(ddbNumberRange))
}

// query is a Query of the key's partition, or half the time of its category in
// the index, and one in ddbFilterEvery has a filter on kind.
func (m *mixer) query(key int) []byte {
	condition, value, index := "pk = :p", "g"+strconv.Itoa(key/ddbLargePartition), ""
	if m.rng.IntN(2) == 0 { //nolint:mnd // one in two
		condition, value, index = "gsi1pk = :p", "c"+strconv.Itoa(key%ddbLargeCategories), `"IndexName":"`+ddbLargeIndex+`",`
	}

	filter, kind := "", ""
	if m.rng.IntN(ddbFilterEvery) == 0 {
		filter, kind = `"FilterExpression":"kind = :k",`, `,":k":{"S":"k`+strconv.Itoa(m.rng.IntN(ddbLargeKinds))+`"}`
	}

	return fmt.Appendf(nil, `{"TableName":%q,%s%s"KeyConditionExpression":%q,`+
		`"ExpressionAttributeValues":{":p":{"S":%q}%s},"Limit":%d}`,
		m.table, index, filter, condition, value, kind, ddbQueryLimit)
}

// scan is a page of a Scan that starts after the key, and one in ddbFilterEvery
// has a filter on kind.
func (m *mixer) scan(key int) []byte {
	if m.rng.IntN(ddbFilterEvery) != 0 {
		return fmt.Appendf(nil, `{"TableName":%q,"ExclusiveStartKey":%s,"Limit":%d}`, m.table, keyJSON(key), ddbScanLimit)
	}

	return fmt.Appendf(nil, `{"TableName":%q,"ExclusiveStartKey":%s,"Limit":%d,"FilterExpression":"kind = :k",`+
		`"ExpressionAttributeValues":{":k":{"S":"k%d"}}}`, m.table, keyJSON(key), ddbScanLimit, m.rng.IntN(ddbLargeKinds))
}

// keyJSON is the primary key of item i.
func keyJSON(i int) []byte {
	return fmt.Appendf(nil, `{"pk":{"S":"g%d"},"sk":{"S":"i%0*d"}}`, i/ddbLargePartition, ddbKeyDigits, i)
}

func appendPut(buf []byte, table string, i int, version uint64) []byte {
	buf = fmt.Appendf(buf, `{"TableName":%q,"Item":`, table)
	buf = appendItem(buf, i, version)

	return append(buf, '}')
}

// appendBatch appends a BatchWriteItem of puts of items first to last-1.
func appendBatch(buf []byte, table string, first, last int, version uint64) []byte {
	buf = fmt.Appendf(buf, `{"RequestItems":{%q:[`, table)

	for i := first; i < last; i++ {
		if i > first {
			buf = append(buf, ',')
		}

		buf = append(buf, `{"PutRequest":{"Item":`...)
		buf = appendItem(buf, i, version)
		buf = append(buf, `}}`...)
	}

	return append(buf, `]}}`...)
}

// itemBuilder writes the DynamoDB JSON of one item.
type itemBuilder struct {
	buf        []byte
	rng        *rand.Rand
	attributes int
}

// attribute starts the attribute name, with a comma after the first.
func (b *itemBuilder) attribute(name string) {
	if b.attributes > 0 {
		b.buf = append(b.buf, ',')
	}

	b.attributes++

	b.buf = append(b.buf, '"')
	b.buf = append(b.buf, name...)
	b.buf = append(b.buf, `":`...)
}

// text writes a string value of n random characters.
func (b *itemBuilder) text(n int) {
	b.buf = append(b.buf, `{"S":"`...)
	b.word(n)
	b.buf = append(b.buf, `"}`...)
}

// word appends n random characters of the alphabet.
func (b *itemBuilder) word(n int) {
	for n > 0 {
		bits := b.rng.Uint64()

		for range min(n, ddbTextWord) {
			b.buf = append(b.buf, ddbAlphabet[bits&(1<<ddbAlphabetBits-1)])
			bits >>= ddbAlphabetBits
			n--
		}
	}
}

func (b *itemBuilder) number(n int) {
	b.buf = append(b.buf, `{"N":"`...)
	b.buf = strconv.AppendInt(b.buf, int64(n), ddbDecimal)
	b.buf = append(b.buf, `"}`...)
}

func (b *itemBuilder) literal(name, value string) {
	b.attribute(name)
	b.buf = append(b.buf, `{"S":"`...)
	b.buf = append(b.buf, value...)
	b.buf = append(b.buf, `"}`...)
}

// shaped writes attribute k past the named ones: a short string, a number, a map
// or a list, in turn.
func (b *itemBuilder) shaped(k int) {
	switch k % ddbShapes {
	case shapeString:
		b.attribute("s" + strconv.Itoa(k))
		b.text(ddbShortMin + b.rng.IntN(ddbShortSpread))
	case shapeNumber:
		b.attribute("x" + strconv.Itoa(k))
		b.number(b.rng.IntN(ddbNumberRange))
	case shapeMap:
		b.attribute("m" + strconv.Itoa(k))
		b.buf = append(b.buf, `{"M":{"a":`...)
		b.text(ddbSmallMin + b.rng.IntN(ddbSmallSpread))
		b.buf = append(b.buf, `,"b":`...)
		b.number(b.rng.IntN(ddbNumberRange))
		b.buf = append(b.buf, `,"c":{"BOOL":`...)
		b.buf = strconv.AppendBool(b.buf, b.rng.IntN(2) == 0) //nolint:mnd // a coin
		b.buf = append(b.buf, `}}}`...)
	default:
		b.attribute("l" + strconv.Itoa(k))
		b.buf = append(b.buf, `{"L":[`...)
		b.text(ddbSmallMin + b.rng.IntN(ddbSmallSpread))
		b.buf = append(b.buf, ',')
		b.number(b.rng.IntN(ddbNumberRange))
		b.buf = append(b.buf, ',')
		b.text(ddbSmallMin + b.rng.IntN(ddbSmallSpread))
		b.buf = append(b.buf, `]}`...)
	}
}

// appendItem appends item i of version v. The item has the four keys, kind, n, a
// body and enough shaped attributes to make 10 to 30 in all, and every byte of
// it follows from i and v.
func appendItem(buf []byte, i int, version uint64) []byte {
	//nolint:gosec // the item's contents, not a secret.
	b := itemBuilder{buf: append(buf, '{'), rng: rand.New(rand.NewPCG(uint64(i), version))}
	attributes := ddbMinAttributes + b.rng.IntN(ddbMaxAttributes-ddbMinAttributes+1)

	b.literal("pk", "g"+strconv.Itoa(i/ddbLargePartition))
	b.literal("sk", fmt.Sprintf("i%0*d", ddbKeyDigits, i))
	b.literal("gsi1pk", "c"+strconv.Itoa(i%ddbLargeCategories))
	b.literal("gsi1sk", fmt.Sprintf("t%0*d", ddbIndexDigits, i*ddbIndexStride%ddbIndexModulus))
	b.literal("kind", "k"+strconv.Itoa(b.rng.IntN(ddbLargeKinds)))
	b.attribute("n")
	b.number(b.rng.IntN(ddbNumberRange))

	// The body's length is spread evenly on a log scale from the small payload to
	// the medium one.
	size := ddbPayloadSmall * math.Exp(b.rng.Float64()*math.Log(float64(ddbPayloadMedium)/ddbPayloadSmall))
	b.attribute("body")
	b.text(int(size))

	for k := range attributes - ddbNamedAttributes {
		b.shaped(k)
	}

	return append(b.buf, '}')
}
