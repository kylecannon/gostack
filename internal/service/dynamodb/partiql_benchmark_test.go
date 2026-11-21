//nolint:unqueryvet // the statements under benchmark are PartiQL, SELECT * included
package dynamodb_test

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkPartiQLSelect is ExecuteStatement's SELECT against the call it
// compiles to: by key against a Query for that one item, and with no WHERE
// against a Scan, and by key through BatchExecuteStatement and ExecuteTransaction,
// on the readTable holding n items of each payload size, on each
// of concurrency simultaneous callers. One op reads the whole result,
// following NextToken or LastEvaluatedKey. Names carry the axes, for example
//
//	BenchmarkPartiQLSelect/items=100/payload=4KiB/read=unkeyed/via=statement/concurrency=16
func BenchmarkPartiQLSelect(b *testing.B) {
	for _, n := range []int{1, 100} {
		for _, payload := range readPayloads {
			b.Run(fmt.Sprintf("items=%d/payload=%s", n, payload.name), func(b *testing.B) {
				s := newStack(b)
				s.ok(b, opCreate, readTable)

				data := strings.Repeat("x", payload.size)
				for i := range n {
					s.ok(b, opPut, `{"TableName":"bench","Item":{"pk":{"S":"a"},"sk":{"N":"`+strconv.Itoa(i)+
						`"},"g":{"S":"G"},"d":{"S":"`+data+`"}}}`)
				}

				selectCells(b, s, n, payload.size)
			})
		}
	}
}

// BenchmarkPartiQLSelectWidth is a Scan through ExecuteStatement by the number
// of paths its SELECT lists and where they are, on a table holding n small
// items: top-level attributes, which the read projects, or paths below one,
// which it reads whole and takes the paths from. No item holds most of them,
// so the cost is the width of the list. One op reads every item. Names carry
// the axes, for example
//
//	BenchmarkPartiQLSelectWidth/items=1000/paths=1000/shape=top/concurrency=16
func BenchmarkPartiQLSelectWidth(b *testing.B) {
	for _, n := range []int{100, 1000} {
		s := newStack(b)
		s.ok(b, opCreate, readTable)

		for i := range n {
			s.ok(b, opPut, `{"TableName":"bench","Item":{"pk":{"S":"a"},"sk":{"N":"`+strconv.Itoa(i)+
				`"},"g":{"S":"G"},"k0":{"S":"x"},"k1":{"S":"x"},"m":{"M":{"k0":{"S":"x"},"k1":{"S":"x"}}}}}`)
		}

		for _, paths := range []int{1, 300, 601, 1000} {
			for _, shape := range []struct{ name, prefix string }{{"top", "k"}, {"nested", "m.k"}} {
				cell := selectCell{
					readUnkeyed, viaStatement, opStatement,
					stmt("SELECT " + selectList(shape.prefix, paths) + " FROM bench"), n,
				}

				for _, workers := range readConcurrency {
					name := fmt.Sprintf("items=%d/paths=%d/shape=%s/concurrency=%d", n, paths, shape.name, workers)
					b.Run(name, func(b *testing.B) {
						b.ReportAllocs()

						for b.Loop() {
							require.NoError(b, fanOutErr(workers, func(int) error { return s.selectAll(b, cell) }))
						}
					})
				}
			}
		}
	}
}

// BenchmarkPartiQLUpdateWidth is an UPDATE through ExecuteStatement by the
// number of SET actions it lists, one item to each of its concurrent callers.
// Names carry the axes, for example
//
//	BenchmarkPartiQLUpdateWidth/actions=600/concurrency=16
func BenchmarkPartiQLUpdateWidth(b *testing.B) {
	for _, actions := range []int{1, 100, 300, 600} {
		for _, workers := range readConcurrency {
			b.Run(fmt.Sprintf("actions=%d/concurrency=%d", actions, workers), func(b *testing.B) {
				s := newStack(b)
				s.ok(b, opCreate, hashTable(benchTable))

				bodies := make([]string, workers)

				for worker := range workers {
					key := "k-" + strconv.Itoa(worker)
					s.ok(b, opPut, `{"TableName":"bench","Item":{"id":{"S":"`+key+`"}}}`)

					sets := make([]string, actions)
					for i := range sets {
						sets[i] = "SET u" + strconv.Itoa(i) + " = 1"
					}

					bodies[worker] = stmt("UPDATE bench " + strings.Join(sets, " ") + " WHERE id = `" + key + "`")
				}

				b.ReportAllocs()

				for b.Loop() {
					require.NoError(b, s.fanOut(b, opStatement, workers, func(worker int) string { return bodies[worker] }))
				}
			})
		}
	}
}

// The read and via axis values the cells share.
const (
	readByKey    = "key"
	readUnkeyed  = "unkeyed"
	viaBatch     = "batch"
	viaStatement = "statement"
)

// selectCell is one read of the matrix: the request and the items it reads.
type selectCell struct {
	read, via, op, body string
	items               int
}

// selectCells runs each read, through a statement and through the call it
// compiles to, at each concurrency.
func selectCells(b *testing.B, s *stack, n, size int) {
	b.Helper()

	cells := []selectCell{
		{readByKey, viaStatement, opStatement, stmt("SELECT * FROM bench WHERE pk = `a` AND sk = 0"), 1},
		{readByKey, "call", opQuery, `{"TableName":"bench","KeyConditionExpression":"pk = :h AND sk = :s",` +
			`"ExpressionAttributeValues":{":h":{"S":"a"},":s":{"N":"0"}}`, 1},
		{readUnkeyed, viaStatement, opStatement, stmt("SELECT * FROM bench"), n},
		{readUnkeyed, "call", opScan, `{"TableName":"bench"`, n},
		{readByKey, viaBatch, opBatchStmt, batch("SELECT * FROM bench WHERE pk = `a` AND sk = 0"), 1},
		{readByKey, "transaction", opTransaction, transaction("SELECT * FROM bench WHERE pk = `a` AND sk = 0"), 1},
	}

	for _, cell := range cells {
		for _, workers := range readConcurrency {
			name := fmt.Sprintf("read=%s/via=%s/concurrency=%d", cell.read, cell.via, workers)
			b.Run(name, func(b *testing.B) {
				b.SetBytes(int64(cell.items * size * workers))
				b.ReportAllocs()

				for b.Loop() {
					require.NoError(b, fanOutErr(workers, func(int) error { return s.selectAll(b, cell) }))
				}
			})
		}
	}
}

// selectAll reads every page of cell and checks it read cell.items items.
func (s *stack) selectAll(b *testing.B, cell selectCell) error {
	b.Helper()

	switch cell.op {
	case opStatement:
	case opBatchStmt, opTransaction:
		return s.readResponses(b, cell)
	default:
		return s.readAll(b, cell.op, cell.body, cell.items)
	}

	count := 0
	body := strings.TrimSuffix(cell.body, "}")
	token := ""

	for {
		got := s.call(b, usEast1, cell.op, body+token+"}")
		if got.status != 200 {
			return fmt.Errorf("%s answered %d: %s", cell.op, got.status, got.body)
		}

		var page struct {
			Items     []json.RawMessage `json:"Items"`
			NextToken string            `json:"NextToken"`
		}

		err := json.Unmarshal([]byte(got.body), &page)
		if err != nil {
			return fmt.Errorf("statement page: %w", err)
		}

		count += len(page.Items)

		if page.NextToken == "" {
			break
		}

		token = `,"NextToken":"` + page.NextToken + `"`
	}

	if count != cell.items {
		return fmt.Errorf("statement read %d items, want %d", count, cell.items)
	}

	return nil
}

// statementItem is one response of a BatchExecuteStatement or ExecuteTransaction.
type statementItem struct {
	Item json.RawMessage `json:"Item"`
}

// readResponses runs a BatchExecuteStatement or ExecuteTransaction cell and
// checks each of its statements read an item.
func (s *stack) readResponses(b *testing.B, cell selectCell) error {
	b.Helper()

	got := s.call(b, usEast1, cell.op, cell.body)
	if got.status != 200 {
		return fmt.Errorf("%s answered %d: %s", cell.op, got.status, got.body)
	}

	var out struct {
		Responses []statementItem `json:"Responses"`
	}

	err := json.Unmarshal([]byte(got.body), &out)
	if err != nil {
		return fmt.Errorf("%s response: %w", cell.op, err)
	}

	read := 0

	for _, r := range out.Responses {
		if r.Item != nil {
			read++
		}
	}

	if read != cell.items {
		return fmt.Errorf("%s read %d items, want %d", cell.op, read, cell.items)
	}

	return nil
}
