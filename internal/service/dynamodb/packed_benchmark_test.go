package dynamodb_test

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// BenchmarkItemReads is the read matrix of packed item storage (SQ-442): each
// read operation, plain, with a filter and with a projection, over items of
// 4 and of 30 attributes, at three concurrencies:
//
//	BenchmarkItemReads/attrs=30/op=Query/form=filter/concurrency=16
//
// The table holds itemReadCount items in one partition. GetItem reads one
// item, BatchGetItem 25, and Query and Scan every item, page after page.
// GetItem and BatchGetItem take no filter. It uses the harness alone, so the
// same file measures a tree that stores items as maps.
//
// A cell whose iteration costs more than maxItemReadUnits (itemReadCell.cost)
// is left out, so that the slowest of its runs is at most 8 ms: of the 60 cells
// 6 are, the Query and the Scan of items of 30 attributes at 64 callers, plain,
// with a filter and with a projection. Their slowest runs were 9.8 to 26.8 ms
// on the gate's runners (ten runs, bench run 45505878281 of PR 84, main
// a0e13bb3). The other 54 took at most 7.3 ms, the slowest being the Query and
// the Scan of items of 4 attributes with a projection at 64 (7.3 and 7.0 ms)
// and of items of 30 with a filter at 16 (7.3 and 7.2 ms). That keeps the whole
// ladder for GetItem and BatchGetItem, every cell at concurrency 1 and 16, and
// at 64 the Query and Scan of the small items. Query and Scan of 30 attributes
// above concurrency 1 stay at 16.

const (
	itemReadCount = 100

	// maxItemReadUnits is the most an iteration of one cell may cost
	// (tooSlowToGate), in units of 0.1 us: 80,000 are 8 ms on a 4 CPU runner.
	// It sits between the dearest cell it keeps, 76,800 units, 7.3 ms, and the
	// cheapest it leaves out, 102,400 units, 9.8 ms.
	maxItemReadUnits = 80_000
)

// The forms of a cell.
const (
	formPlain      = "plain"
	formProjection = "projection"
)

var itemReadConcurrency = []int{1, 16, 64}

// itemReadCell is one operation and form: the operation, its body without
// the closing brace, and the items one call reads.
type itemReadCell struct {
	op, form, body string
	items          int
}

// cost is what one iteration of the cell costs for workers callers on items of
// attrs attributes, in units of 0.1 us per item read, as the slowest runs of
// Query and Scan at 16 and 64 callers on the gate's runners (4 CPUs) read: 5 for
// a plain read of items of 4, which the store serves from their bytes, 12 for a
// filter or projection of them, which decodes them, 45 for items of 30 and 16
// for a projection of those. BatchGetItem of the small items looks up each key,
// and costs twice that; of the wide ones it is no dearer than a Query (3.2 us an
// item plain at 64 callers, 3.1 with a projection).
func (c itemReadCell) cost(attrs, workers int) int {
	var perItem int

	switch {
	case attrs > itemReadSmall && c.form == formProjection:
		perItem = 16
	case attrs > itemReadSmall:
		perItem = 45
	case c.form == formPlain:
		perItem = 5
	default:
		perItem = 12
	}

	if c.op == opBatchGet && attrs <= itemReadSmall {
		perItem *= 2
	}

	return c.items * workers * perItem
}

// itemReadSmall is the attribute count of the small items; the others are
// wide.
const itemReadSmall = 4

func BenchmarkItemReads(b *testing.B) {
	for _, attrs := range []int{itemReadSmall, 30} {
		b.Run(fmt.Sprintf("attrs=%d", attrs), func(b *testing.B) {
			s := newStack(b)
			s.ok(b, opCreate, readTable)

			for i := range itemReadCount {
				s.ok(b, opPut, `{"TableName":"bench","Item":`+itemReadItem(i, attrs)+`}`)
			}

			for _, cell := range itemReadCells() {
				for _, workers := range itemReadConcurrency {
					if tooSlowToGate(cell.cost(attrs, workers), maxItemReadUnits) {
						continue
					}

					name := fmt.Sprintf("op=%s/form=%s/concurrency=%d", cell.op, cell.form, workers)
					b.Run(name, func(b *testing.B) {
						b.ReportAllocs()

						for b.Loop() {
							err := fanOutErr(workers, func(int) error { return itemRead(b, s, cell) })
							if err != nil {
								b.Fatal(err)
							}
						}
					})
				}
			}
		})
	}
}

// itemReadItem is item i of the table: the keys, the string a1 and the number
// n0, then strings, numbers, lists and maps up to attrs attributes.
func itemReadItem(i, attrs int) string {
	var item strings.Builder

	item.WriteString(`{"pk":{"S":"a"},"sk":{"N":"` + strconv.Itoa(i) + `"}`)
	fmt.Fprintf(&item, `,"a1":{"S":"value of item %d, some text"},"n0":{"N":"%d.25"}`, i, i)

	for a := 4; a < attrs; a++ {
		switch a % 4 {
		case 0:
			fmt.Fprintf(&item, `,"x%d":{"N":"%d.5"}`, a, i*a)
		case 1:
			fmt.Fprintf(&item, `,"x%d":{"S":"value %d of item %d, some text"}`, a, a, i)
		case 2:
			fmt.Fprintf(&item, `,"x%d":{"L":[{"N":"%d"},{"S":"x"},{"BOOL":true}]}`, a, i)
		default:
			fmt.Fprintf(&item, `,"x%d":{"M":{"k":{"S":"v%d"},"n":{"N":"%d"}}}`, a, i, a)
		}
	}

	item.WriteString(`}`)

	return item.String()
}

func itemReadCells() []itemReadCell {
	const (
		table      = `{"TableName":"bench"`
		query      = table + `,"KeyConditionExpression":"pk = :h"`
		filter     = `,"FilterExpression":"attribute_exists(a1) AND n0 >= :z"`
		projection = `,"ProjectionExpression":"pk, sk, a1"`
		key        = `{"pk":{"S":"a"},"sk":{"N":"7"}}`
	)

	keys := make([]string, 25)
	for i := range keys {
		keys[i] = `{"pk":{"S":"a"},"sk":{"N":"` + strconv.Itoa(i*3) + `"}}`
	}

	batch := `{"RequestItems":{"bench":{"Keys":[` + strings.Join(keys, ",") + `]`

	return []itemReadCell{
		{opGet, formPlain, table + `,"Key":` + key, 1},
		{opGet, formProjection, table + `,"Key":` + key + projection, 1},
		{opBatchGet, formPlain, batch + `}}`, len(keys)},
		{opBatchGet, formProjection, batch + projection + `}}`, len(keys)},
		{opQuery, formPlain, query + `,"ExpressionAttributeValues":{":h":{"S":"a"}}`, itemReadCount},
		{opQuery, "filter", query + filter + `,"ExpressionAttributeValues":{":h":{"S":"a"},":z":{"N":"0"}}`, itemReadCount},
		{opQuery, formProjection, query + projection + `,"ExpressionAttributeValues":{":h":{"S":"a"}}`, itemReadCount},
		{opScan, formPlain, table, itemReadCount},
		{opScan, "filter", table + filter + `,"ExpressionAttributeValues":{":z":{"N":"0"}}`, itemReadCount},
		{opScan, formProjection, table + projection, itemReadCount},
	}
}

// itemRead makes the cell's call, every page of it for Query and Scan, and
// checks it read the cell's items.
func itemRead(b *testing.B, s *stack, cell itemReadCell) error {
	b.Helper()

	if cell.op == opQuery || cell.op == opScan {
		return s.readAll(b, cell.op, cell.body, cell.items)
	}

	got := s.call(b, usEast1, cell.op, cell.body+"}")
	if got.status != 200 || strings.Count(got.body, `"pk":`) != cell.items {
		return fmt.Errorf("%s answered %d: %.200s", cell.op, got.status, got.body)
	}

	return nil
}
