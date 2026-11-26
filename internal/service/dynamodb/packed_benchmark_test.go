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
// is left out, so that the slowest of its runs is at most 8 ms, and at most
// 6.5 ms on a runner as slow as the slowest seen: the runners differ by 1.65
// times over this family (GetItem of 4 attributes at 64 callers took 0.5 ms in
// bench run 45727425239 of PR 84 and 0.9 ms in run 45732740061). Of the 60
// cells 18 are left out; the slowest runs of ten in run 45732740061 (the
// slower), 4 CPUs, were:
//
//   - Query and Scan of 30 attributes at 16 callers, plain and with a filter:
//     8.6 and 11.6 ms plain, 10.6 and 10.8 ms with a filter;
//   - Query and Scan of 4 attributes with a filter at 64: 11.4 and 7.6 ms, and
//     with a projection 14.0 and 10.8 ms;
//   - BatchGetItem at 64, plain and with a projection: 6.7 and 7.6 ms of 4
//     attributes, 7.8 and 7.4 ms of 30;
//   - Query and Scan of 30 attributes at 64, which that run did not time: 9.8
//     to 26.8 ms in run 45505878281 (the faster).
//
// What it keeps is GetItem and every cell at concurrency 1, every cell at 16
// but the plain and filter reads of 30 attributes (4.8 ms at most, the Scan of
// 30 attributes with a projection), and at 64 the plain Query and Scan of 4
// (5.4 ms at most). Each class keeps a cell: Query and Scan of 30 attributes
// above concurrency 1 are the projections at 16, BatchGetItem above 1 is 16.

const (
	itemReadCount = 100

	// maxItemReadUnits is the most an iteration of one cell may cost
	// (tooSlowToGate), in units of 0.1 us: 65,000 are 6.5 ms on a 4 CPU
	// runner of run 45732740061. It sits between the dearest cell it keeps,
	// 57,600 units, 5.4 ms (the Scan of 4 attributes at 64), and the cheapest
	// it leaves out, 80,000 units, 6.7 to 7.8 ms (BatchGetItem at 64).
	maxItemReadUnits = 65_000
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
// attrs attributes, in units of 0.1 us per item read, as the slowest runs on the
// slower runner (4 CPUs, bench run 45732740061 of PR 84) read at 16 and 64
// callers: 9 for a plain Query or Scan of items of 4, which the store serves
// from their bytes (0.6 to 0.9 us), 22 for a filter or projection of them, which
// decodes them (1.2 to 2.2 us), 73 for a plain read or a filter of items of 30
// (5.4 to 7.3 us) and 30 for a projection of those (2.2 to 3.0 us).
// BatchGetItem looks up each key, and costs 50 an item whatever the item (4.2 to
// 4.9 us at 64 callers).
func (c itemReadCell) cost(attrs, workers int) int {
	var perItem int

	switch {
	case c.op == opBatchGet:
		perItem = 50
	case attrs > itemReadSmall && c.form == formProjection:
		perItem = 30
	case attrs > itemReadSmall:
		perItem = 73
	case c.form == formPlain:
		perItem = 9
	default:
		perItem = 22
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
