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
//
// The cells run concurrency by concurrency, each on a stack of its own
// (itemConcurrency).
func BenchmarkPartiQLSelect(b *testing.B) {
	for _, workers := range readConcurrency {
		for _, n := range []int{1, 100} {
			for _, payload := range readPayloads {
				selectCells(b, n, payload, workers)
			}
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
//
// A cell whose iteration costs more than selectWidthBudget (selectShape.units)
// is left out: the slowest of a cell's runs has to be at most 8 ms. On the
// gate's runners (ten runs, bench run 45505878281 of PR 84, main a0e13bb3) the
// cells left out took 11.4 to 385 ms: every list of 300 paths or more at
// concurrency 16, the nested lists of 300 and more paths over 1,000 items at
// concurrency 1 (16.7 to 52.8 ms), the nested list of 1,000 paths over 100
// items at 1 (11.9 ms), and the lists of one path over 1,000 items at 16 (11.5
// ms top, 14.8 ms nested). What it keeps is the width of the list at
// concurrency 1 (to 1,000 paths over 1,000 items of the top shape, 7.7 ms, and
// 601 over 100 of the nested, 5.5 ms), and concurrency 16 for the lists of one
// path and of ten, the multi-path list at 16 that the wider lists lose over
// 100 items. The list of ten is not timed on the runners: on a Mac of 10 CPUs
// the slowest of five runs was 3.1 ms at 16 over 100 items (nested), 2.1 ms at
// 1 over 1,000 items (local).
//
// The cells run concurrency by concurrency, each on a table of its own
// (itemConcurrency).
func BenchmarkPartiQLSelectWidth(b *testing.B) {
	for _, workers := range readConcurrency {
		for _, n := range []int{100, 1000} {
			for _, paths := range []int{1, 10, 300, 601, 1000} {
				for _, shape := range selectShapes {
					if tooSlowToGate(shape.units(n, paths, workers), selectWidthBudget) {
						continue
					}

					cell := selectCell{
						readUnkeyed, viaStatement, opStatement,
						stmt("SELECT " + selectList(shape.prefix, paths) + " FROM bench"), n,
					}
					name := fmt.Sprintf("items=%d/paths=%d/shape=%s/concurrency=%d", n, paths, shape.name, workers)

					b.Run(name, func(b *testing.B) {
						s := newStack(b)
						s.ok(b, opCreate, readTable)

						for i := range n {
							s.ok(b, opPut, `{"TableName":"bench","Item":{"pk":{"S":"a"},"sk":{"N":"`+strconv.Itoa(i)+
								`"},"g":{"S":"G"},"k0":{"S":"x"},"k1":{"S":"x"},"m":{"M":{"k0":{"S":"x"},"k1":{"S":"x"}}}}}`)
						}

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

// selectWidthBudget is the most units (selectShape.units) an iteration of
// BenchmarkPartiQLSelectWidth costs, about 8 ms: the model reads 0.4 to 1.8 ms
// for 1,000 units over the cells, so the budget sits between the widest cells
// it keeps and the narrowest it leaves out. The slowest of ten runs on the
// gate's runners (bench run 45505878281 of PR 84), for the cells at its edge:
//
//	top, 1,000 items, 1,000 paths, 1 worker (5,000 units)    7.7 ms  kept
//	                (7.4 ms in run 45732740061, the slower runner, the dearest kept)
//	nested, 100 items, 601 paths, 1 worker (6,260)           5.5 ms  kept
//	nested, 100 items, 1,000 paths, 1 worker (10,350)       11.9 ms  left out
//	top, 1,000 items, 1 path, 16 workers (16,064)           11.5 ms  left out
//	nested, 1,000 items, 1 path, 16 workers (17,056)        14.8 ms  left out
//	nested, 1,000 items, 300 paths, 1 worker (20,950)       16.7 ms  left out
//	top, 100 items, 300 paths, 16 workers (20,800)          20.1 ms  left out
const selectWidthBudget = 8_000

// selectShape is a shape of the list BenchmarkPartiQLSelectWidth reads: paths
// prefix0, prefix1 and on, top-level attributes the read projects, or ones
// below an attribute, which it reads whole (whole) and takes the paths from.
type selectShape struct {
	name, prefix string
	whole        bool
}

var selectShapes = []selectShape{{"top", "k", false}, {"nested", "m.k", true}}

// units is what an iteration of BenchmarkPartiQLSelectWidth costs, in visits
// of one item (about 0.5 us on a 4 CPU runner): every worker visits every item
// and compiles the list, a path costing 4 units, and a path below an attribute
// is also read from each item whole, 1 unit for 16 items and paths.
func (s selectShape) units(items, paths, workers int) int {
	perCall := items + 4*paths
	if s.whole {
		perCall += items * paths / 16
	}

	return workers * perCall
}

// maxNarrowResults is the most values an iteration of BenchmarkPartiQLSelectNarrow
// builds, 0.7 us each on a 4 CPU runner.
const maxNarrowResults = 8_000

// BenchmarkPartiQLSelectNarrow is BenchmarkPartiQLSelectWidth's nested shape for
// a list the items hold all of: n items whose map has k0 to k29, read through
// the first `paths` of them. The result of each item is as wide as the list, so
// what it costs is the map each result is built in. Names carry the axes, for
// example
//
//	BenchmarkPartiQLSelectNarrow/items=100/paths=10/concurrency=16
//
// A cell whose iteration builds more than maxNarrowResults values (items times
// paths times workers) is left out so that the slowest of a cell's runs stays at
// most 8 ms. On the gate's runners (ten runs, bench run 45505878281 of PR 84,
// main a0e13bb3) 30 paths at 16 workers, 48,000 values, took 19.5 to 20.9 ms,
// and 10 paths at 16, 16,000, took 8.7 to 11.3 ms; 3 paths at 16, 4,800, took
// 3.2 to 3.5 ms, so the budget is 8,000. The 30-path list is kept at
// concurrency 1, and at 16 the list of 3 is the multi-path SELECT over many
// callers.
func BenchmarkPartiQLSelectNarrow(b *testing.B) {
	const held = 30

	members := make([]string, held)
	for i := range members {
		members[i] = `"k` + strconv.Itoa(i) + `":{"S":"x"}`
	}

	const n = 100

	s := newStack(b)
	s.ok(b, opCreate, readTable)

	for i := range n {
		s.ok(b, opPut, `{"TableName":"bench","Item":{"pk":{"S":"a"},"sk":{"N":"`+strconv.Itoa(i)+
			`"},"m":{"M":{`+strings.Join(members, ",")+`}}}}`)
	}

	for _, paths := range []int{1, 3, 10, 30} {
		cell := selectCell{
			readUnkeyed, viaStatement, opStatement, stmt("SELECT " + selectList("m.k", paths) + " FROM bench"), n,
		}

		for _, workers := range readConcurrency {
			if tooSlowToGate(n*paths*workers, maxNarrowResults) {
				continue
			}

			b.Run(fmt.Sprintf("items=%d/paths=%d/concurrency=%d", n, paths, workers), func(b *testing.B) {
				b.ReportAllocs()

				for b.Loop() {
					require.NoError(b, fanOutErr(workers, func(int) error { return s.selectAll(b, cell) }))
				}
			})
		}
	}
}

// maxUpdateWidthActions is the most SET actions an iteration of
// BenchmarkPartiQLUpdateWidth lists over its workers: 1,000 are about 4.5 ms on a
// 4 CPU runner (600 at 1 worker took 3.2 ms).
const maxUpdateWidthActions = 1_000

// BenchmarkPartiQLUpdateWidth is an UPDATE through ExecuteStatement by the
// number of SET actions it lists, one item to each of its concurrent callers.
// Names carry the axes, for example
//
//	BenchmarkPartiQLUpdateWidth/actions=600/concurrency=1
//
// A cell whose iteration lists more than maxUpdateWidthActions actions over its
// workers is left out, so the slowest of a cell's runs stays at most 8 ms. On
// the gate's runners (ten runs) 100 actions at 16 took 6.8 to 7.3 ms in bench
// run 45505878281 of PR 84 (main a0e13bb3) and 9.9 to 10.6 ms in run
// 45471851078 (PR 79's head), a spread of 1.5 times for one cell, so it is left
// out with its neighbours, 300 at 16, 15.4 to 16.4 ms, and 600 at 16, 26.6 to
// 28.1 ms (23.5 to 26.0 and 41.6 to 57.7 ms in the other run). 600 at 1 took 3.0
// to 3.2 ms (4.1 to 5.0 in the other) and is kept. The multi-action UPDATE at
// 16 is the list of 10, not timed on the runners: 1.3 ms on a Mac of 10 CPUs,
// the slowest of five runs (local).
func BenchmarkPartiQLUpdateWidth(b *testing.B) {
	for _, actions := range []int{1, 10, 100, 300, 600} {
		for _, workers := range readConcurrency {
			if tooSlowToGate(actions*workers, maxUpdateWidthActions) {
				continue
			}

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
// compiles to, at workers callers when the iteration carries at most
// maxIterationReadBytes, the budget of a Query and a Scan of the same bytes
// (BenchmarkReads): 100 items of 4 KiB at 16 callers, 6.5 MB, took 6.0 to
// 8.4 ms through the call and 6.8 to 9.9 ms through the statement on the gate's
// runners (ten runs, bench run 45505878281 of PR 84), and are left out. Each
// cell reads n items of payload on a table of its own.
func selectCells(b *testing.B, n int, payload itemPayload, workers int) {
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
		if tooSlowToGate(cell.items*payload.size*workers, maxIterationReadBytes) {
			continue
		}

		name := fmt.Sprintf("items=%d/payload=%s/read=%s/via=%s/concurrency=%d",
			n, payload.name, cell.read, cell.via, workers)
		b.Run(name, func(b *testing.B) {
			s := filledReads(b, n, payload.size)

			b.SetBytes(int64(cell.items * payload.size * workers))
			b.ReportAllocs()

			for b.Loop() {
				require.NoError(b, fanOutErr(workers, func(int) error { return s.selectAll(b, cell) }))
			}
		})
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
