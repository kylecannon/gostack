package dynamodb_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The read and update benchmark matrix (docs/standards.md, "Benchmarks"):
// Query and Scan through the real gateway, in process, by result size, item
// size, with and without a filter, on the table and on a global index, and
// UpdateItem by expression size. Sub-benchmark names carry the axes, for
// example
//
//	BenchmarkReads/items=100/payload=4KiB/op=Query/target=gsi/filter=on/concurrency=16
//	BenchmarkReads/items=100/payload=4KiB/text=cjk/op=Query/target=table/filter=off/concurrency=1
//
// One iteration reads the whole result, following LastEvaluatedKey across the
// 1 MB pages, on each of concurrency simultaneous callers. The filter keeps
// every item, so it adds its evaluation and not a smaller result. 500 items of
// 4 KiB are three pages; 10,000 items made ops of 60 to 340 ms, too slow for
// the benchstat gate's ten runs a row. A cell that would carry more than
// maxIterationReadItems or maxIterationReadBytes in one iteration is left out
// (readTooSlow): 100 or 500 items of 4 KiB at concurrency 16, and 500 of 100 B
// with a filter at 16. What that leaves of the reads over 1 MB pages is
// concurrency 1; pagedReads keeps a paged read at 16 by a Limit of 25.
//
// The text cells hold non-ASCII or escape-dense strings, which json quotes
// differently from ASCII, and only on a plain Query of 100 items: the text
// changes nothing but the encoding of the result.

var (
	readSizes       = []int{1, 100, 500}
	readPayloads    = []itemPayload{{"100B", 100}, {"4KiB", 4 << 10}}
	readConcurrency = []int{1, 16}
	readSerial      = readConcurrency[:1]
	readParallel    = readConcurrency[1:]
	updateActions   = []int{1, 10, 50}
	readFilters     = []struct{ name, expr string }{{"off", ""}, {"on", "attribute_exists(d)"}}
)

// targetGSI is the target axis value of a read on the global index.
const targetGSI = "gsi"

// readTable is a CreateTable body for bench: keyed by pk and the number sk,
// with a global index on g that projects every attribute.
const readTable = `{"TableName":"bench","BillingMode":"PAY_PER_REQUEST",` +
	`"KeySchema":[{"AttributeName":"pk","KeyType":"HASH"},{"AttributeName":"sk","KeyType":"RANGE"}],` +
	`"AttributeDefinitions":[{"AttributeName":"pk","AttributeType":"S"},{"AttributeName":"sk","AttributeType":"N"},` +
	`{"AttributeName":"g","AttributeType":"S"}],` +
	`"GlobalSecondaryIndexes":[{"IndexName":"gall","KeySchema":[{"AttributeName":"g","KeyType":"HASH"}],` +
	`"Projection":{"ProjectionType":"ALL"}}]}`

// BenchmarkReads runs its cells at concurrency 1 (the matrix, then the text
// cells) before the ones at 16 (the matrix, then the paged reads), each cell on
// a stack of its own: a read at 1 of the 1 MB pages leaves large buffers in the
// json encoder, and the cells at 16 grow more (itemConcurrency).
func BenchmarkReads(b *testing.B) {
	readMatrix(b, readSerial)
	readTexts(b)
	readMatrix(b, readParallel)
	pagedReads(b)
}

// readMatrix runs readCells for every number of items and payload at each of
// concurrencies, in order.
func readMatrix(b *testing.B, concurrencies []int) {
	b.Helper()

	for _, workers := range concurrencies {
		for _, n := range readSizes {
			for _, payload := range readPayloads {
				readCells(b, n, payload, workers)
			}
		}
	}
}

// readTexts runs the text cells: a Query of 100 items of 4 KiB of each text, at
// concurrency 1.
func readTexts(b *testing.B) {
	b.Helper()

	const textItems = 100

	payload := readPayloads[1]

	for _, text := range itemTexts {
		name := fmt.Sprintf("items=%d/payload=%s/text=%s/op=Query/target=table/filter=off/concurrency=1",
			textItems, payload.name, text.name)

		b.Run(name, func(b *testing.B) {
			s := newStack(b)
			s.ok(b, opCreate, readTable)

			data, err := json.Marshal(text.value(payload.size))
			require.NoError(b, err)

			for i := range textItems {
				s.ok(b, opPut, `{"TableName":"bench","Item":{"pk":{"S":"a"},"sk":{"N":"`+strconv.Itoa(i)+
					`"},"g":{"S":"G"},"d":{"S":`+string(data)+`}}}`)
			}

			body := readBody(opQuery, "table", "")

			b.SetBytes(int64(textItems * payload.size))
			b.ReportAllocs()

			for b.Loop() {
				require.NoError(b, s.readAll(b, opQuery, body, textItems))
			}
		})
	}
}

// filledReads is a stack holding readTable, filled with n items of size bytes
// under one partition key.
func filledReads(b *testing.B, n, size int) *stack {
	b.Helper()

	s := newStack(b)
	s.ok(b, opCreate, readTable)

	data := strings.Repeat("x", size)
	for i := range n {
		s.ok(b, opPut, `{"TableName":"bench","Item":{"pk":{"S":"a"},"sk":{"N":"`+strconv.Itoa(i)+
			`"},"g":{"S":"G"},"d":{"S":"`+data+`"}}}`)
	}

	return s
}

// pagedReads reads 100 items of 100 B in pages of 25 (Limit), four calls for
// each of 16 callers, each carrying the start key of the one before: the
// paged read under concurrency, which the 1 MB pages leave out: a result of
// more than 1 MB over 16 callers is more than 16 MB, 2.5 times the 6.5 MB cells
// that took up to 9.3 ms on the runners. A new cell, not timed on the runners:
// on a Mac of 10 CPUs the slowest of five runs was 2.8 ms (Query) and 2.5 ms
// (Scan) (local).
func pagedReads(b *testing.B) {
	const (
		n       = 100
		limit   = 25
		workers = 16
	)

	payload := readPayloads[0]

	for _, op := range []string{opQuery, opScan} {
		body := readBody(op, "table", "") + fmt.Sprintf(`,"Limit":%d`, limit)
		name := fmt.Sprintf("items=%d/payload=%s/limit=%d/op=%s/target=table/filter=off/concurrency=%d",
			n, payload.name, limit, op, workers)

		b.Run(name, func(b *testing.B) {
			s := filledReads(b, n, payload.size)

			b.SetBytes(int64(n * payload.size * workers))
			b.ReportAllocs()

			for b.Loop() {
				require.NoError(b, fanOutErr(workers, func(int) error { return s.readAll(b, op, body, n) }))
			}
		})
	}
}

// What one iteration of a read cell may carry, counted over its workers
// (tooSlowToGate). The slowest of a cell's runs has to be at most 8 ms. The
// times are the slowest of ten runs on the gate's runners (bench run 45505878281
// of PR 84, main a0e13bb3):
//
//   - maxIterationReadItems counts an item a filter evaluates twice. 500 items
//     of 100 B at 16 workers are 8,000 without a filter, 4.3 to 6.7 ms, and
//     16,000 with one, 4.6 to 10.0 ms: the four filtered cells took 7.9 (Query
//     on the table), 8.0 (Scan on the index), 9.2 and 10.0 ms. The two of them
//     under 8 ms are left out with the other two, which they share the budget
//     with, as is 100 items of 4 KiB at 16 below. The filtered cells are left
//     out, the unfiltered ones kept.
//   - maxIterationReadBytes: 100 items of 4 KiB at 16 workers, 6.5 MB, took
//     6.0 to 9.3 ms (the Scan without a filter, on the table and on the index,
//     7.7 and 7.9 ms, shares the budget with the Query without a filter,
//     8.4 and 8.2 ms, and the filtered cells, 9.0 to 9.3) and 500 of them 26 to
//     40 ms. 500 at 1 worker, 2.0 MB, took 3.0 to 5.0 ms, and are kept. The
//     unkeyed reads of BenchmarkPartiQLSelect take the same budget.
const (
	maxIterationReadItems = 8_000
	maxIterationReadBytes = 4 << 20
)

// readTooSlow reports whether one iteration of a cell reading n items of size
// bytes on each of workers callers, through the filter expression filter (none
// when empty), is over what a read cell carries.
func readTooSlow(n, size, workers int, filter string) bool {
	items := n * workers
	if filter != "" {
		items *= 2
	}

	return tooSlowToGate(items, maxIterationReadItems) || tooSlowToGate(n*size*workers, maxIterationReadBytes)
}

// readCells runs every operation, target and filter at workers callers over
// n items of payload, on a table of its own, that readTooSlow leaves in.
func readCells(b *testing.B, n int, payload itemPayload, workers int) {
	b.Helper()

	for _, op := range []string{opQuery, opScan} {
		for _, target := range []string{"table", targetGSI} {
			for _, filter := range readFilters {
				if readTooSlow(n, payload.size, workers, filter.expr) {
					continue
				}

				body := readBody(op, target, filter.expr)
				name := fmt.Sprintf("items=%d/payload=%s/op=%s/target=%s/filter=%s/concurrency=%d",
					n, payload.name, op, target, filter.name, workers)

				b.Run(name, func(b *testing.B) {
					s := filledReads(b, n, payload.size)

					b.SetBytes(int64(n * payload.size * workers))
					b.ReportAllocs()

					for b.Loop() {
						require.NoError(b, fanOutErr(workers, func(int) error { return s.readAll(b, op, body, n) }))
					}
				})
			}
		}
	}
}

// indexTable is readTable with two more global indexes on g that project less:
// gkeys holds only the keys, and ginc adds d.
const indexTable = `{"TableName":"bench","BillingMode":"PAY_PER_REQUEST",` +
	`"KeySchema":[{"AttributeName":"pk","KeyType":"HASH"},{"AttributeName":"sk","KeyType":"RANGE"}],` +
	`"AttributeDefinitions":[{"AttributeName":"pk","AttributeType":"S"},{"AttributeName":"sk","AttributeType":"N"},` +
	`{"AttributeName":"g","AttributeType":"S"}],` +
	`"GlobalSecondaryIndexes":[{"IndexName":"gkeys","KeySchema":[{"AttributeName":"g","KeyType":"HASH"}],` +
	`"Projection":{"ProjectionType":"KEYS_ONLY"}},` +
	`{"IndexName":"ginc","KeySchema":[{"AttributeName":"g","KeyType":"HASH"}],` +
	`"Projection":{"ProjectionType":"INCLUDE","NonKeyAttributes":["d"]}}]}`

// BenchmarkIndexProjection reads a global index that does not project every
// attribute, with a ProjectionExpression the index holds and one it does not,
// which is refused: the cost of the check that decides which.
func BenchmarkIndexProjection(b *testing.B) {
	const items = 100

	s := newStack(b)
	s.ok(b, opCreate, indexTable)

	data := strings.Repeat("x", 100)
	for i := range items {
		s.ok(b, opPut, `{"TableName":"bench","Item":{"pk":{"S":"a"},"sk":{"N":"`+strconv.Itoa(i)+
			`"},"g":{"S":"G"},"d":{"S":"`+data+`"}}}`)
	}

	for _, op := range []string{opQuery, opScan} {
		for _, c := range []struct{ index, held, refused string }{
			{"gkeys", "pk, sk, g", "d"},
			{"ginc", "pk, d", "x"},
		} {
			for _, outcome := range []struct{ name, projection string }{{"held", c.held}, {"refused", c.refused}} {
				body := indexReadBody(op, c.index, outcome.projection)
				name := fmt.Sprintf("op=%s/index=%s/projection=%s", op, c.index, outcome.name)

				b.Run(name, func(b *testing.B) {
					b.ReportAllocs()

					for b.Loop() {
						if outcome.name == "refused" {
							got := s.call(b, usEast1, op, body+"}")
							require.Equal(b, http.StatusBadRequest, got.status, got.body)

							continue
						}

						require.NoError(b, s.readAll(b, op, body, items))
					}
				})
			}
		}
	}
}

// indexReadBody is a Query or a Scan of index with a ProjectionExpression,
// without its closing brace.
func indexReadBody(op, index, projection string) string {
	body := `{"TableName":"bench","IndexName":"` + index + `","ProjectionExpression":"` + projection + `"`

	if op == opQuery {
		body += `,"KeyConditionExpression":"g = :h","ExpressionAttributeValues":{":h":{"S":"G"}}`
	}

	return body
}

// readBody is the request of one cell without its closing brace, so a start
// key member can follow. filter is the FilterExpression, or none when empty.
func readBody(op, target, filter string) string {
	var body strings.Builder

	body.WriteString(`{"TableName":"bench"`)

	if target == targetGSI {
		body.WriteString(`,"IndexName":"gall"`)
	}

	values := ""

	if op == opQuery {
		if target == targetGSI {
			body.WriteString(`,"KeyConditionExpression":"g = :h"`)
			values = `":h":{"S":"G"}`
		} else {
			body.WriteString(`,"KeyConditionExpression":"pk = :h"`)
			values = `":h":{"S":"a"}`
		}
	}

	if filter != "" {
		body.WriteString(`,"FilterExpression":"` + filter + `"`)
	}

	if values != "" {
		body.WriteString(`,"ExpressionAttributeValues":{` + values + `}`)
	}

	return body.String()
}

// readAll reads every page of body and checks it returned want items.
func (s *stack) readAll(b *testing.B, op, body string, want int) error {
	b.Helper()

	count := 0
	start := ""

	for {
		got := s.call(b, usEast1, op, body+start+"}")
		if got.status != 200 {
			return fmt.Errorf("%s answered %d: %s", op, got.status, got.body)
		}

		var page struct {
			Count            int             `json:"Count"`
			LastEvaluatedKey json.RawMessage `json:"LastEvaluatedKey"`
		}

		err := json.Unmarshal([]byte(got.body), &page)
		if err != nil {
			return fmt.Errorf("%s page: %w", op, err)
		}

		count += page.Count

		if page.LastEvaluatedKey == nil {
			break
		}

		start = `,"ExclusiveStartKey":` + string(page.LastEvaluatedKey)
	}

	if count != want {
		return fmt.Errorf("%s read %d items, want %d", op, count, want)
	}

	return nil
}

// fanOutErr runs fn on workers goroutines at once and returns the first error.
func fanOutErr(workers int, fn func(worker int) error) error {
	errs := make(chan error, workers)

	for worker := range workers {
		go func() { errs <- fn(worker) }()
	}

	var first error

	for range workers {
		err := <-errs
		if err != nil && first == nil {
			first = err
		}
	}

	return first
}

func BenchmarkUpdateItem(b *testing.B) {
	benchUpdate(b, newStack)
	b.Run(persistOn, func(b *testing.B) { benchUpdate(b, persistedStack) })
	benchUpdatePayload(b)
}

// benchUpdate runs the UpdateItem matrix on the stacks mk builds.
func benchUpdate(b *testing.B, mk func(testing.TB) *stack) {
	b.Helper()

	for _, actions := range updateActions {
		for _, workers := range readConcurrency {
			b.Run(fmt.Sprintf("actions=%d/concurrency=%d", actions, workers), func(b *testing.B) {
				s := mk(b)
				s.ok(b, opCreate, hashTable(benchTable))

				sets := make([]string, actions)
				for i := range sets {
					sets[i] = "a" + strconv.Itoa(i) + " = :v"
				}

				expr := "SET " + strings.Join(sets, ", ")
				bodies := make([]string, workers)

				for worker := range workers {
					bodies[worker] = fmt.Sprintf(`{"TableName":%q,"Key":{"id":{"S":"k-%d"}},"UpdateExpression":%q,`+
						`"ExpressionAttributeValues":{":v":{"N":"1"}}}`, benchTable, worker, expr)
				}

				b.SetBytes(int64(len(expr) * workers))
				b.ReportAllocs()

				for b.Loop() {
					require.NoError(b, s.fanOut(b, opUpdateItem, workers, func(worker int) string { return bodies[worker] }))
				}
			})
		}
	}
}
