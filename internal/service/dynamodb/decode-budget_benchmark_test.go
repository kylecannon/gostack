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
//     limit below. BenchmarkDenseAdmission measures the smallest such bodies.
//   - A cell whose iteration costs more than maxDecodeMicros
//     (decodeBody.microsPerKB) is left out, so that the gate makes at least 50
//     iterations and the slowest of its runs is at most 8 ms (docs/standards.md,
//     "Every feature ships its benchmark matrix"). On the gate's runners (ten
//     runs, bench run 45505878281 of PR 84, main a0e13bb3) the cells left out
//     took: the 400 KB string item at 64, 13.1 to 19.9 ms; the 1 MB string batch
//     at 16, 9.5 to 13.3 ms; and, at concurrency 1, the dense item of 10,000
//     numbers (100 KB), 9.2 to 13.2 ms, and the 1 MB dense batch, 7.6 to
//     10.0 ms, which are measured at half that size. Earlier, on another run, the
//     400 KB dense item took 47 ms and the 15 MB batches 366 ms (dense) and
//     13.6 ms (string) at concurrency 1 and 208 ms (string) at 16. A body past the
//     budget cannot be shrunk under the limit: the smallest, about 56,000
//     numbers, takes 18 ms. The payload axis is covered by the widest cells
//     under the limit: the 500 KB dense batch and the 400 KB string (6.7 ms at
//     16 callers, the slowest kept). The classes the cuts would empty keep a
//     cell each: PutItem at concurrency 64 is the string of 25 KB, and
//     BatchWriteItem above concurrency 1 is 250 KB of strings, at 16.
//     Those two and the dense bodies at half their size, 5,000 numbers (50 KB)
//     and 12 items (500 KB), were not timed on the runners. The slowest of five
//     runs on a Mac of 10 CPUs (local) were 2.1 ms (dense item), 2.7 ms (dense
//     batch), 1.6 ms (25 KB at 64) and 4.5 ms (250 KB at 16); the Mac took 0.7
//     to 1.6 times the runner's time on the cells both ran. By the runner's times
//     for twice their size the dense bodies are the nearest the limit, 4.6 to
//     6.6 ms and 3.8 to 5.0 ms.

const (
	shapeDense  = "dense"
	shapeString = "string"
)

// maxDecodeMicros is the most one iteration of a cell may cost, in
// microseconds of decoding, so that its slowest run is at most 8 ms.
// decodeBody.microsPerKB prices a body, per kilobyte of it for each caller, as
// the slowest run on the gate's runners for a kilobyte of the body: 1 for a
// string, which 400 KB at 16 callers, 6,400 microseconds, took 4.1 to 6.6 ms
// for, 130 for a list of small values (10,000 numbers, 100 KB, 13.2 ms) and 10
// for the batch of records (1 MB, 10.0 ms).
const maxDecodeMicros = 7_000

// denseNumbers is how many {"N":"1"} the dense PutItem holds: a 50 KB body that
// decodes into 0.8 MB, a 26th of what DynamoDB's 400 KB item limit allows
// (numbers).
const denseNumbers = 5_000

// denseBatchItems is how many items the dense batch holds, of the 25 a batch
// takes: 12 are about 500 KB.
const denseBatchItems = 12

// decodeBody is one body of the matrix, priced microsPerKB for each caller.
type decodeBody struct {
	operation, name, shape string
	microsPerKB            int
	body                   func(worker int) string
}

// decodeShapes are the bodies: a PutItem of a list of small values (dense) and
// of strings of 400 KB and 25 KB, and BatchWriteItem as a batch of records
// (dense) and of strings of 1 MB and 250 KB.
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
		{opPut, "PutItem50KB", shapeDense, 130, func(w int) string {
			return missing(denseItem(fmt.Sprint("p", w), numberList(denseNumbers)))
		}},
		{opPut, "PutItem400KB", shapeString, 1, func(w int) string { return missing(text(fmt.Sprint("p", w), 400_000)) }},
		{opPut, "PutItem25KB", shapeString, 1, func(w int) string { return missing(text(fmt.Sprint("p", w), 25_000)) }},
		{opBatchWrite, "500KB", shapeDense, 10, denseBatchToMissing},
		{opBatchWrite, "1MB", shapeString, 1, batch(func(w, i int) string {
			return text(fmt.Sprint("b", w, "-", i), 40_000)
		})},
		{opBatchWrite, "250KB", shapeString, 1, batch(func(w, i int) string {
			return text(fmt.Sprint("b", w, "-", i), 10_000)
		})},
	}
}

// denseBatchToMissing is convoyBatch of denseBatchItems items, aimed at the
// table that does not exist.
func denseBatchToMissing(worker int) string {
	return strings.Replace(textBatchOf(worker, denseBatchItems, 36_000, 36_000), `"burst"`, `"missing"`, 1)
}

func BenchmarkDecodeBudget(b *testing.B) {
	// Every body sent together is under the budget, so none takes the dense lane
	// and none is throttled for it. What a cell sends at once is at most 6.5 MB
	// (the 400 KB string at 16), a tenth of the in-flight bound, so no worker
	// waits for room under it today; the wait is long so that a cell that grows
	// to one that does is measured, served one worker after another, and not
	// throttled.
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
			size := len(shape.body(0)) * workers

			if (workers > 1 && dense) || tooSlowToGate(size/1000*shape.microsPerKB, maxDecodeMicros) {
				continue
			}

			bodies := make([]string, workers)
			for worker := range workers {
				bodies[worker] = shape.body(worker)
			}

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
// in-flight bound set, at concurrency 1: the body finds no queue, the calls main
// makes. The bodies are lists nested to DynamoDB's depth limit (nestedLists),
// the cheapest to decode of the bodies that pass the budget: the budget is
// protocol.DecodeSlack (16 MiB) and twice the body, so the smallest such body,
// 342 lists in 85 KB, decodes 16 MiB, and a list of numbers past the budget
// takes 18 ms. Sub-benchmark names carry the axes, for example
//
//	BenchmarkDenseAdmission/body=88KB/concurrency=1
//
// Cells left out, and why: a body that waits in the dense queue
// (protocol.WaitDense) for another's to give back the lane, which is
// concurrency 2 of the 88 KB body, and 4 of it, 2 of the 150 KB one, which
// serve their bodies one after another. No body is smaller than 85 KB, so two
// of them cost two of the 4.4 ms (median) one takes: the 88 KB body at 2 took
// 7.3 to 9.2 ms (median 7.6) on the gate's runners, 4 CPUs, ten runs, bench run
// 45727425239 of PR 84, over the 8 ms a cell is held to, and a 150 KB body at 1,
// 7.0 to 9.4 ms (median 7.4), which is now 100 KB (400 lists) at about 5 ms. The
// queue's waits are held by tests, not a benchmark:
// Test_decodeBudget_twoDenseSendersInAClosedLoopAreAllServed
// (decode-budget-queue_test.go) and the queue's own in internal/protocol. The
// 88 KB cell kept took 4.2 to 5.2 ms in that run. The 100 KB cell is not timed
// on the runners: a Mac of 10 CPUs took 3.4 to 3.6 ms for it and 3.0 to 3.3 ms
// for the 88 KB body, where the 150 KB body took 5.1 to 5.3 ms (local, five
// runs), which the runner timed at 7.0 to 9.4 ms, so 100 KB is about 5 ms
// there.
func BenchmarkDenseAdmission(b *testing.B) {
	roomyMemoryLimit(b)
	protocol.SetInFlight(protocol.DefaultInFlight)
	b.Cleanup(func() { protocol.SetInFlight(0) })

	for _, size := range []struct {
		name  string
		lists int
	}{{"88KB", 350}, {"100KB", 400}} {
		bodies := []string{`{"TableName":"missing","Item":` + denseItem("n0", nestedLists(size.lists)) + `}`}

		require.True(b, passesBudget(b, opPut, bodies[0]), "the body takes the dense lane")

		b.Run(fmt.Sprintf("body=%s/concurrency=1", size.name), func(b *testing.B) {
			s := newStack(b)

			b.SetBytes(int64(len(bodies[0])))
			b.ReportAllocs()

			for b.Loop() {
				require.NoError(b, notFound(b, s, opPut, bodies))
			}
		})
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
