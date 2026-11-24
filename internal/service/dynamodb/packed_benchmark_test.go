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

const itemReadCount = 100

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

func BenchmarkItemReads(b *testing.B) {
	for _, attrs := range []int{4, 30} {
		b.Run(fmt.Sprintf("attrs=%d", attrs), func(b *testing.B) {
			s := newStack(b)
			s.ok(b, opCreate, readTable)

			for i := range itemReadCount {
				s.ok(b, opPut, `{"TableName":"bench","Item":`+itemReadItem(i, attrs)+`}`)
			}

			for _, cell := range itemReadCells() {
				for _, workers := range itemReadConcurrency {
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
