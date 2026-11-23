package dynamodb_test

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
	"github.com/stretchr/testify/require"
)

// The decode budget's benchmark matrix (docs/standards.md, "Benchmarks"):
// bodies of the shapes that cost the most to admit and decode, dense with small
// values and plain strings, sent with the in-flight bound set, through the real
// gateway, in process. Sub-benchmark names carry the axes, for example
//
//	BenchmarkDecodeBudget/op=BatchWriteItem/body=1MB/shape=string/concurrency=16
//
// Every call goes to a table that does not exist, so each decodes its body whole
// and is answered ResourceNotFoundException, and nothing is stored: what is
// measured is reading, admitting and decoding the body. One iteration is one
// call from each of the concurrent workers.
//
// Cells left out, and why:
//
//   - A body whose decoding passes protocol.DecodeBudget (passesBudget) is sent
//     at concurrency 1 only. Such a body takes the process's one dense lane
//     (protocol.TakeDenseLane), and another that arrives while one is walked and
//     decoded waits its turn in the dense queue (protocol.WaitDense), so an
//     iteration of 16 or 64 of them takes 16 or 64 times one, past the time
//     limit below. The queue's matrix is BenchmarkDenseAdmission.
//   - A cell whose slowest operation over three runs at the gate's benchtime
//     passes 8 ms is left out, so that the gate makes at least 50 iterations
//     (docs/standards.md, "Every feature ships its benchmark matrix");
//     decodeBody.widest is the most workers a body is sent with. The slowest
//     runs of the cells left out: the 400 KB dense item 47 ms and the 15 MB
//     batches 366 ms (dense) and 13.6 ms (string) at concurrency 1, 208 ms
//     (string) at 16; the 1 MB dense batch 17 ms at 16 and the 1 MB string
//     batch 13.1 ms at 64. A body past the budget cannot be shrunk under the
//     limit: the smallest, about 56,000 numbers, takes 18 ms. The dense item is
//     measured at 10,000 numbers instead, under the budget. The payload axis is
//     covered by the widest cells under the limit: the 1 MB batch and the 400 KB
//     string.

const (
	shapeDense  = "dense"
	shapeString = "string"
)

// denseNumbers is how many {"N":"1"} the dense PutItem holds: a 100 KB body that
// decodes into 1.6 MB, a tenth of what DynamoDB's 400 KB item limit allows
// (numbers).
const denseNumbers = 10_000

// decodeBody is one body of the matrix, sent with at most widest workers.
type decodeBody struct {
	operation, name, shape string
	widest                 int
	body                   func(worker int) string
}

// decodeShapes are the bodies: a PutItem of a list of small values (dense) and
// of a 400 KB string, and BatchWriteItem of 1 MB, as a batch of records
// (dense) and of strings.
func decodeShapes() []decodeBody {
	missing := func(item string) string { return `{"TableName":"missing","Item":` + item + `}` }

	batch := func(item func(worker, i int) string) func(worker int) string {
		return func(worker int) string {
			requests := make([]string, batchItems)
			for i := range requests {
				requests[i] = `{"PutRequest":{"Item":` + item(worker, i) + `}}`
			}

			return `{"RequestItems":{"missing":[` + strings.Join(requests, ",") + `]}}`
		}
	}

	text := func(id string, n int) string {
		return `{"id":{"S":"` + id + `"},"t":{"S":"` + strings.Repeat("x", n) + `"}}`
	}

	return []decodeBody{
		{opPut, "PutItem100KB", shapeDense, 1, func(w int) string {
			return missing(denseItem(fmt.Sprint("p", w), numberList(denseNumbers)))
		}},
		{opPut, "PutItem400KB", shapeString, 64, func(w int) string { return missing(text(fmt.Sprint("p", w), 400_000)) }},
		{opBatchWrite, "1MB", shapeDense, 1, convoyBatchToMissing},
		{opBatchWrite, "1MB", shapeString, 16, batch(func(w, i int) string {
			return text(fmt.Sprint("b", w, "-", i), 40_000)
		})},
	}
}

// convoyBatchToMissing is convoyBatch aimed at the table that does not exist.
func convoyBatchToMissing(worker int) string {
	return strings.Replace(convoyBatch(worker), `"burst"`, `"missing"`, 1)
}

func BenchmarkDecodeBudget(b *testing.B) {
	// Every body sent together is under the budget, so none takes the dense lane
	// and none is throttled for it. What a cell sends at once is at most 16 MB,
	// a quarter of the in-flight bound, so no worker waits for room under it
	// today; the wait is long so that a cell that grows to one that does is
	// measured, served one worker after another, and not throttled.
	roomyMemoryLimit(b)
	protocol.SetInFlight(protocol.DefaultInFlight)
	protocol.SetBodyWait(5 * time.Minute)
	b.Cleanup(func() {
		protocol.SetInFlight(0)
		protocol.SetBodyWait(protocol.DefaultBodyWait)
	})

	for _, shape := range decodeShapes() {
		dense := passesBudget(b, shape.operation, shape.body(0))

		for _, workers := range itemConcurrency {
			if workers > shape.widest || (workers > 1 && dense) {
				continue
			}

			bodies := make([]string, workers)
			for worker := range workers {
				bodies[worker] = shape.body(worker)
			}

			size := len(bodies[0]) * workers

			b.Run(fmt.Sprintf("op=%s/body=%s/shape=%s/concurrency=%d", shape.operation, shape.name,
				shape.shape, workers), func(b *testing.B) {
				s := newStack(b)

				b.SetBytes(int64(size))
				b.ReportAllocs()

				for b.Loop() {
					require.NoError(b, notFound(b, s, shape.operation, bodies))
				}
			})
		}
	}
}

// passesBudget reports whether decoding body, a call to operation, is charged
// more than protocol.DecodeBudget of its bytes: the bodies that take the dense
// lane (protocol.TakeDenseLane). It walks the body as the gateway does, against
// that budget and holding nothing, so it follows what a body costs and not the
// shape it is named for.
func passesBudget(b *testing.B, operation, body string) bool {
	b.Helper()

	budget := protocol.DecodeBudget(len(body))

	var err *awserr.Error

	switch operation {
	case opPut:
		_, _, err = awsjson.DecodeWithin[api.PutItemInput]([]byte(body), budget, nil)
	case opBatchWrite:
		_, _, err = awsjson.DecodeWithin[api.BatchWriteItemInput]([]byte(body), budget, nil)
	default:
		b.Fatalf("no decoder for %s", operation)
	}

	require.True(b, err == nil || err == awsjson.ErrTooLarge, "%s body does not decode: %v", operation, err)

	return err != nil
}

// BenchmarkDenseAdmission is the dense queue's matrix: a PutItem whose body
// passes protocol.DecodeBudget, sent through the real gateway with the
// in-flight bound set, from concurrency workers at once. One iteration is one
// call from each worker. At concurrency 1 the body finds no queue, the calls
// main makes; at 2 one worker's body waits in the dense queue
// (protocol.WaitDense) for the other's to give back the lane, and none is
// throttled. The bodies are lists nested to DynamoDB's depth limit
// (nestedLists), the cheapest to decode of the bodies that pass the budget:
// the smallest such body, 342 lists in 85 KB, is served in 3 ms, where a list
// of numbers past the budget takes 18 ms. Sub-benchmark names carry the axes,
// for example
//
//	BenchmarkDenseAdmission/body=88KB/concurrency=2
//
// Cells left out, and why: concurrency 4 of the 88 KB body and 2 of the 150 KB
// one, which serve their bodies one after another: four of 3 ms and two of
// 5 ms, near the 10 ms limit. The slowest operations of the cells kept, over
// three runs at the gate's benchtime: 3.0 ms and 4.9 ms for the 88 KB body at
// concurrency 1 and 2, and 5.0 ms for the 150 KB one.
func BenchmarkDenseAdmission(b *testing.B) {
	roomyMemoryLimit(b)
	protocol.SetInFlight(protocol.DefaultInFlight)
	b.Cleanup(func() { protocol.SetInFlight(0) })

	for _, size := range []struct {
		name   string
		lists  int
		widest int
	}{{"88KB", 350, 2}, {"150KB", 600, 1}} {
		for _, workers := range []int{1, 2} {
			if workers > size.widest {
				continue
			}

			bodies := make([]string, workers)
			for worker := range workers {
				bodies[worker] = `{"TableName":"missing","Item":` +
					denseItem(fmt.Sprint("n", worker), nestedLists(size.lists)) + `}`
			}

			require.True(b, passesBudget(b, opPut, bodies[0]), "the body takes the dense lane")

			b.Run(fmt.Sprintf("body=%s/concurrency=%d", size.name, workers), func(b *testing.B) {
				s := newStack(b)

				b.SetBytes(int64(len(bodies[0]) * workers))
				b.ReportAllocs()

				for b.Loop() {
					require.NoError(b, notFound(b, s, opPut, bodies))
				}
			})
		}
	}
}

// notFound sends the bodies at once, one worker each, and returns the first
// answer that is not ResourceNotFoundException.
func notFound(b *testing.B, s *stack, operation string, bodies []string) error {
	b.Helper()

	errs := make([]error, len(bodies))

	var wg sync.WaitGroup

	for worker, body := range bodies {
		wg.Go(func() {
			got := s.call(b, usEast1, operation, body)
			if got.status != http.StatusBadRequest || !strings.Contains(got.body, "ResourceNotFoundException") {
				errs[worker] = fmt.Errorf("%s answered %d: %s", operation, got.status, got.body[:min(len(got.body), 200)])
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
