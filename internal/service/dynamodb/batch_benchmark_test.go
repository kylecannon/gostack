package dynamodb_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The batch and transaction benchmark matrix (docs/standards.md,
// "Benchmarks"), through the real gateway, in process. Sub-benchmark names
// carry the axes, for example
//
//	BenchmarkBatchWriteItem/payload=4KiB/concurrency=16
//	BenchmarkTransactWriteItems/actions=10/concurrency=64
//
// One iteration is one call from each of the concurrent workers, each on its
// own items, so ns/op is per that many calls.

var (
	// The item benchmarks' two smaller payloads, and 32 KiB in place of their
	// largest: 25 items of it make a 10 MB request.
	batchPayloads    = []itemPayload{itemPayloads[0], itemPayloads[1], {"32KiB", 32 << 10}}
	transactActions  = []int{1, 10, 100}
	transactItemSize = 100
)

// batchWriteBody is a BatchWriteItem body putting 25 items of worker, each
// with a d of size bytes.
func batchWriteBody(worker, size int) string {
	d := strings.Repeat("x", size)
	requests := make([]string, maxBenchBatch)

	for i := range requests {
		requests[i] = fmt.Sprintf(`{"PutRequest":{"Item":{"id":{"S":"k-%d-%d"},"d":{"S":%q}}}}`, worker, i, d)
	}

	return `{"RequestItems":{"` + benchTable + `":[` + strings.Join(requests, ",") + `]}}`
}

// batchGetBody is a BatchGetItem body reading the 25 items batchWriteBody
// puts for worker.
func batchGetBody(worker int) string {
	keys := make([]string, maxBenchBatch)

	for i := range keys {
		keys[i] = fmt.Sprintf(`{"id":{"S":"k-%d-%d"}}`, worker, i)
	}

	return `{"RequestItems":{"` + benchTable + `":{"Keys":[` + strings.Join(keys, ",") + `]}}}`
}

const (
	// maxBenchBatch is the most items one BatchWriteItem takes. BatchGetItem
	// reads the same 25, so its rows line up with the writes'.
	maxBenchBatch = 25

	// One iteration is a call from every worker, so its cost grows with
	// workers times the work of one call. A cell whose iteration would carry
	// more than these is left out (tooSlowToGate): at make bench's 500 ms it
	// runs under 50 iterations, too few for benchstat to judge (SQ-302), and
	// the slowest of its runs has to be at most 8 ms. The slowest of ten runs
	// on the gate's runners (4 CPUs, bench run 45505878281 of PR 84, main
	// a0e13bb3) of what each limit leaves out and keeps:
	//
	//   - maxIterationBatchBytes, for BatchGetItem: the 32 KiB batches at
	//     concurrency 64, 51 MB of items, which main already left out. Kept:
	//     32 KiB at 16 (12.8 MB), 2.8 ms, and 4 KiB at 64, 4.3 ms.
	//   - maxIterationWriteBytes, for BatchWriteItem, 4 MiB of the bytes of its
	//     items and writeItemOverhead for each. A write costs about three
	//     times a read of the same bytes, and a small item costs more than its
	//     bytes. Left out: the 32 KiB batches at 16 (15.3 ms), the 4 KiB ones
	//     at 64 (11.4 ms) and the 100 B ones at 64 (8.5 ms, with the bytes
	//     alone 160 KB). Kept: 4 KiB at 16, 4.1 ms, and 100 B at 16, 1.9 ms.
	//     BatchWriteItem has no cell at concurrency 64 for that, which
	//     TransactWriteItems, BatchGetItem and PutItem still have.
	//   - maxIterationActions, for TransactGetItems: 100 actions at 16 and 64,
	//     which main already left out. Kept: 10 at 64, 2.0 ms.
	//   - maxIterationWriteActions, for TransactWriteItems: 10 actions at 64
	//     (12.4 ms) and 100 at 16 and 64. Kept: 10 at 16, 3.1 ms, and 100 at
	//     1, 1.6 ms.
	maxIterationBatchBytes   = 16 << 20
	maxIterationWriteBytes   = 4 << 20
	maxIterationActions      = 640
	maxIterationWriteActions = 320

	// writeItemOverhead is the bytes a BatchWriteItem is charged for each item
	// besides its own: 4 KiB, the cost of the item's write at 5 us on a 4 CPU
	// runner (100 B at 64 workers, 1,600 items, took 6.3 to 8.5 ms) against
	// 0.8 us for each KiB it holds.
	writeItemOverhead = 4 << 10
)

// batchMatrix runs one sub-benchmark for every batch payload at every
// concurrency whose iteration, items of the payload's size and overhead each
// besides, carries at most budget, each on a fresh stack holding the table
// bench. prepare runs before the timer for each worker.
func batchMatrix(
	b *testing.B,
	operation string,
	budget, overhead int,
	prepare func(b *testing.B, s *stack, worker, size int),
	body func(worker, size int) string,
) {
	b.Helper()

	for _, payload := range batchPayloads {
		for _, workers := range itemConcurrency {
			if tooSlowToGate(maxBenchBatch*workers*(payload.size+overhead), budget) {
				continue
			}

			b.Run(fmt.Sprintf("payload=%s/concurrency=%d", payload.name, workers), func(b *testing.B) {
				s := newStack(b)
				s.ok(b, opCreate, hashTable(benchTable))

				bodies := make([]string, workers)
				for worker := range workers {
					prepare(b, s, worker, payload.size)
					bodies[worker] = body(worker, payload.size)
				}

				b.SetBytes(int64(payload.size * maxBenchBatch * workers))
				b.ReportAllocs()

				for b.Loop() {
					require.NoError(b, s.fanOut(b, operation, workers, func(worker int) string { return bodies[worker] }))
				}
			})
		}
	}
}

// BenchmarkBatchWriteItem: each call puts 25 of the worker's items.
func BenchmarkBatchWriteItem(b *testing.B) {
	batchMatrix(b, opBatchWrite, maxIterationWriteBytes, writeItemOverhead, func(*testing.B, *stack, int, int) {},
		batchWriteBody)
}

// BenchmarkBatchGetItem: each call reads the 25 items of the worker that one
// BatchWriteItem put before the timer starts.
//
// One more row reads 12 items of 399 KiB of newlines: each newline is two
// bytes of json, so the body is about 9 MiB, a large json response buffer
// grown by a read of escape-dense text. It had 40 items, about as much as one
// BatchGetItem returns and 31 MiB of json, past the 32 MiB of large buffers the
// json encoder keeps, and took 14.0 to 27.8 ms (items=40) on the gate's runners
// (ten runs, bench run 45505878281 of PR 84), 0.35 to 0.7 ms an item. At 12
// items it took 4.0 to 5.2 ms there. What the row no longer reads is the
// buffer kept past that 32 MiB, and no cell measures what keeping it costs now:
// the encoder's tests (internal/protocol/awsjson/buffers_internal_test.go) hold
// what it keeps, not its time.
func BenchmarkBatchGetItem(b *testing.B) {
	batchMatrix(b, opBatchGet, maxIterationBatchBytes, 0, func(b *testing.B, s *stack, worker, size int) {
		s.ok(b, opBatchWrite, batchWriteBody(worker, size))
	}, func(worker, _ int) string { return batchGetBody(worker) })

	b.Run("payload=399KiB/items=12/text=newlines/concurrency=1", func(b *testing.B) {
		const items = 12

		s := newStack(b)
		s.ok(b, opCreate, hashTable(benchTable))

		d := strings.Repeat("\n", itemPayloads[2].size)
		keys := make([]string, items)

		for i := range keys {
			s.ok(b, opPut, putValue(i, d))
			keys[i] = fmt.Sprintf(`{"id":{"S":"k-%d"}}`, i)
		}

		body := `{"RequestItems":{"` + benchTable + `":{"Keys":[` + strings.Join(keys, ",") + `]}}}`

		responses, ok := s.ok(b, opBatchGet, body)["Responses"].(map[string]any)
		require.True(b, ok, "a call returns Responses")
		require.Len(b, responses[benchTable], items, "a call returns every item")

		b.SetBytes(int64(items * len(d)))
		b.ReportAllocs()

		for b.Loop() {
			require.Equal(b, http.StatusOK, s.call(b, usEast1, opBatchGet, body).status)
		}
	})
}

// transactMatrix runs one sub-benchmark for every number of actions at every
// concurrency whose iteration holds at most budget of them, each on a fresh
// stack holding the table bench, with body(worker, actions) the request of each
// worker.
func transactMatrix(b *testing.B, operation string, budget int, body func(worker, actions int) string) {
	b.Helper()

	for _, actions := range transactActions {
		for _, workers := range itemConcurrency {
			if tooSlowToGate(actions*workers, budget) {
				continue
			}

			b.Run(fmt.Sprintf("actions=%d/concurrency=%d", actions, workers), func(b *testing.B) {
				s := newStack(b)
				s.ok(b, opCreate, hashTable(benchTable))

				d := strings.Repeat("x", transactItemSize)

				for worker := range workers {
					for i := range actions {
						s.ok(b, opPut, fmt.Sprintf(`{"TableName":%q,"Item":{"id":{"S":"k-%d-%d"},"d":{"S":%q}}}`,
							benchTable, worker, i, d))
					}
				}

				bodies := make([]string, workers)
				for worker := range workers {
					bodies[worker] = body(worker, actions)
				}

				b.SetBytes(int64(transactItemSize * actions * workers))
				b.ReportAllocs()

				for b.Loop() {
					require.NoError(b, s.fanOut(b, operation, workers, func(worker int) string { return bodies[worker] }))
				}
			})
		}
	}
}

// BenchmarkTransactWriteItems: each action an Update of one of the worker's
// items under a condition that holds, so every call evaluates and writes all
// of them.
func BenchmarkTransactWriteItems(b *testing.B) {
	transactMatrix(b, opTransactWrite, maxIterationWriteActions, func(worker, actions int) string {
		list := make([]string, actions)
		for i := range list {
			list[i] = fmt.Sprintf(`{"Update":{"TableName":%q,"Key":{"id":{"S":"k-%d-%d"}},`+
				`"ConditionExpression":"attribute_exists(id)","UpdateExpression":"SET n = :n",`+
				`"ExpressionAttributeValues":{":n":{"N":"%d"}}}}`, benchTable, worker, i, i)
		}

		return `{"TransactItems":[` + strings.Join(list, ",") + `]}`
	})
}

// BenchmarkTransactGetItems: each get one of the worker's items.
func BenchmarkTransactGetItems(b *testing.B) {
	transactMatrix(b, opTransactGet, maxIterationActions, func(worker, actions int) string {
		list := make([]string, actions)
		for i := range list {
			list[i] = fmt.Sprintf(`{"Get":{"TableName":%q,"Key":{"id":{"S":"k-%d-%d"}}}}`, benchTable, worker, i)
		}

		return `{"TransactItems":[` + strings.Join(list, ",") + `]}`
	})
}
