package dynamodb_test

import (
	"fmt"
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

// maxBenchBatch is the most items one BatchWriteItem takes.
const maxBenchBatch = 25

func BenchmarkBatchWriteItem(b *testing.B) {
	for _, payload := range batchPayloads {
		for _, workers := range itemConcurrency {
			b.Run(fmt.Sprintf("payload=%s/concurrency=%d", payload.name, workers), func(b *testing.B) {
				s := newStack(b)
				s.ok(b, opCreate, hashTable(benchTable))

				bodies := make([]string, workers)
				for worker := range workers {
					bodies[worker] = batchWriteBody(worker, payload.size)
				}

				b.SetBytes(int64(payload.size * maxBenchBatch * workers))
				b.ReportAllocs()

				for b.Loop() {
					require.NoError(b, s.fanOut(b, opBatchWrite, workers, func(worker int) string { return bodies[worker] }))
				}
			})
		}
	}
}

// transactMatrix runs one sub-benchmark for every number of actions at every
// concurrency, each on a fresh stack holding the table bench, with body(worker,
// actions) the request of each worker.
func transactMatrix(b *testing.B, operation string, body func(worker, actions int) string) {
	b.Helper()

	for _, actions := range transactActions {
		for _, workers := range itemConcurrency {
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
	transactMatrix(b, opTransactWrite, func(worker, actions int) string {
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
	transactMatrix(b, opTransactGet, func(worker, actions int) string {
		list := make([]string, actions)
		for i := range list {
			list[i] = fmt.Sprintf(`{"Get":{"TableName":%q,"Key":{"id":{"S":"k-%d-%d"}}}}`, benchTable, worker, i)
		}

		return `{"TransactItems":[` + strings.Join(list, ",") + `]}`
	})
}
