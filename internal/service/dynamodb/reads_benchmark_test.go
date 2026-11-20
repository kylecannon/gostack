package dynamodb_test

import (
	"encoding/json"
	"fmt"
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
//
// One iteration reads the whole result, following LastEvaluatedKey across the
// 1 MB pages, on each of concurrency simultaneous callers. The filter keeps
// every item, so it adds its evaluation and not a smaller result.

var (
	readSizes       = []int{1, 100, 10000}
	readPayloads    = []itemPayload{{"100B", 100}, {"4KiB", 4 << 10}}
	readConcurrency = []int{1, 16}
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

func BenchmarkReads(b *testing.B) {
	for _, n := range readSizes {
		for _, payload := range readPayloads {
			b.Run(fmt.Sprintf("items=%d/payload=%s", n, payload.name), func(b *testing.B) {
				s := newStack(b)
				s.ok(b, opCreate, readTable)

				data := strings.Repeat("x", payload.size)
				for i := range n {
					s.ok(b, opPut, `{"TableName":"bench","Item":{"pk":{"S":"a"},"sk":{"N":"`+strconv.Itoa(i)+
						`"},"g":{"S":"G"},"d":{"S":"`+data+`"}}}`)
				}

				readCells(b, s, n, payload.size)
			})
		}
	}
}

// readCells runs every operation, target, filter and concurrency over the
// filled table.
func readCells(b *testing.B, s *stack, n, size int) {
	b.Helper()

	for _, op := range []string{opQuery, opScan} {
		for _, target := range []string{"table", targetGSI} {
			for _, filter := range readFilters {
				body := readBody(op, target, filter.expr)

				for _, workers := range readConcurrency {
					name := fmt.Sprintf("op=%s/target=%s/filter=%s/concurrency=%d", op, target, filter.name, workers)
					b.Run(name, func(b *testing.B) {
						b.SetBytes(int64(n * size * workers))
						b.ReportAllocs()

						for b.Loop() {
							require.NoError(b, fanOutErr(workers, func(int) error { return s.readAll(b, op, body, n) }))
						}
					})
				}
			}
		}
	}
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
	for _, actions := range updateActions {
		for _, workers := range readConcurrency {
			b.Run(fmt.Sprintf("actions=%d/concurrency=%d", actions, workers), func(b *testing.B) {
				s := newStack(b)
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
