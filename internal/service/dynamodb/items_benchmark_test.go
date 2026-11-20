package dynamodb_test

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// The item benchmark matrix (docs/standards.md, "Benchmarks"): PutItem and
// GetItem through the real gateway, in process, across item size and
// concurrency. Sub-benchmark names carry the axes, for example
//
//	BenchmarkPutItem/payload=4KiB/concurrency=16
//
// At concurrency 16 one iteration is 16 simultaneous calls, each on its own
// key, so ns/op and the byte rate are per sixteen calls. The largest payload
// keeps the item just under DynamoDB's 400 KB limit.

var (
	itemPayloads    = []itemPayload{{"100B", 100}, {"4KiB", 4 << 10}, {"399KiB", 399 << 10}}
	itemConcurrency = []int{1, 16, 64}
)

const benchTable = "bench"

type itemPayload struct {
	name string
	size int
}

// itemMatrix runs one sub-benchmark for every payload at every concurrency,
// each on a fresh stack holding the table bench, keyed by id.
func itemMatrix(b *testing.B, run func(b *testing.B, s *stack, p itemPayload, workers int)) {
	b.Helper()
	itemMatrixOn(b, newStack, run)
}

// itemMatrixOn is itemMatrix on the stacks mk builds.
func itemMatrixOn(
	b *testing.B, mk func(testing.TB) *stack, run func(b *testing.B, s *stack, p itemPayload, workers int),
) {
	b.Helper()

	for _, payload := range itemPayloads {
		for _, workers := range itemConcurrency {
			b.Run(fmt.Sprintf("payload=%s/concurrency=%d", payload.name, workers), func(b *testing.B) {
				s := mk(b)
				s.ok(b, opCreate, hashTable(benchTable))
				run(b, s, payload, workers)
			})
		}
	}
}

// putBody is a PutItem body for the item keyed k-worker with a d of size bytes.
func putBody(worker, size int) string {
	return fmt.Sprintf(`{"TableName":%q,"Item":{"id":{"S":"k-%d"},"d":{"S":%q}}}`,
		benchTable, worker, strings.Repeat("x", size))
}

// getBody is a GetItem body for the item keyed k-worker.
func getBody(worker int) string {
	return fmt.Sprintf(`{"TableName":%q,"Key":{"id":{"S":"k-%d"}}}`, benchTable, worker)
}

// fanOut sends operation on workers goroutines at once, worker i with body(i),
// and returns the first answer that is not a 200.
func (s *stack) fanOut(b *testing.B, operation string, workers int, body func(worker int) string) error {
	b.Helper()

	errs := make([]error, workers)

	var wg sync.WaitGroup

	for worker := range workers {
		wg.Go(func() {
			got := s.call(b, usEast1, operation, body(worker))
			if got.status != http.StatusOK {
				errs[worker] = fmt.Errorf("%s answered %d: %s", operation, got.status, got.body)
			}
		})
	}

	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return err
		}
	}

	return nil
}

func BenchmarkPutItem(b *testing.B) {
	itemMatrix(b, benchPut)
	b.Run(persistOn, func(b *testing.B) { itemMatrixOn(b, persistedStack, benchPut) })
}

func benchPut(b *testing.B, s *stack, p itemPayload, workers int) {
	b.Helper()

	bodies := make([]string, workers)
	for worker := range workers {
		bodies[worker] = putBody(worker, p.size)
	}

	b.SetBytes(int64(p.size * workers))
	b.ReportAllocs()

	for b.Loop() {
		require.NoError(b, s.fanOut(b, opPut, workers, func(worker int) string { return bodies[worker] }))
	}
}

func BenchmarkGetItem(b *testing.B) {
	itemMatrix(b, func(b *testing.B, s *stack, p itemPayload, workers int) {
		for worker := range workers {
			s.ok(b, opPut, putBody(worker, p.size))
		}

		bodies := make([]string, workers)
		for worker := range workers {
			bodies[worker] = getBody(worker)
		}

		b.SetBytes(int64(p.size * workers))
		b.ReportAllocs()

		for b.Loop() {
			require.NoError(b, s.fanOut(b, opGet, workers, func(worker int) string { return bodies[worker] }))
		}
	})
}
