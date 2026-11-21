package dynamodb_test

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/stretchr/testify/require"
)

// The decode budget's benchmark matrix (docs/standards.md, "Benchmarks"):
// bodies of the shapes that cost the most to admit and decode, dense with small
// values and plain strings, sent with the in-flight bound set, through the real
// gateway, in process. Sub-benchmark names carry the axes, for example
//
//	BenchmarkDecodeBudget/op=BatchWriteItem/body=1MB/shape=dense/concurrency=16
//
// Every call goes to a table that does not exist, so each decodes its body whole
// and is answered ResourceNotFoundException, and nothing is stored: what is
// measured is reading, admitting and decoding the body. One iteration is one
// call from each of the concurrent workers.
//
// A cell whose iteration would send more than maxDecodeIterationBytes is left
// out, as the batch rows leave theirs (SQ-302): a 15 MB body at concurrency 64
// is a gigabyte in one process and half a minute an iteration.

const maxDecodeIterationBytes = 256 << 20

const (
	shapeDense  = "dense"
	shapeString = "string"
)

// decodeBody is one body of the matrix.
type decodeBody struct {
	operation, name, shape string
	body                   func(worker int) string
}

// decodeShapes are the bodies: a PutItem of 400 KB of item, and BatchWriteItem
// of 1 MB and of about 15 MB, each as a list of small values (dense) and as
// strings.
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
		{opPut, "PutItem400KB", shapeDense, func(w int) string {
			return missing(denseItem(fmt.Sprint("p", w), numberList(numbers)))
		}},
		{opPut, "PutItem400KB", shapeString, func(w int) string { return missing(text(fmt.Sprint("p", w), 400_000)) }},
		{opBatchWrite, "1MB", shapeDense, convoyBatchToMissing},
		{opBatchWrite, "1MB", shapeString, batch(func(w, i int) string { return text(fmt.Sprint("b", w, "-", i), 40_000) })},
		{opBatchWrite, "15MB", shapeDense, batch(func(w, i int) string {
			return denseItem(fmt.Sprint("b", w, "-", i), numberList(batchNumbers))
		})},
		{opBatchWrite, "15MB", shapeString, batch(func(w, i int) string {
			return text(fmt.Sprint("b", w, "-", i), 600_000)
		})},
	}
}

// convoyBatchToMissing is convoyBatch aimed at the table that does not exist.
func convoyBatchToMissing(worker int) string {
	return strings.Replace(convoyBatch(worker), `"burst"`, `"missing"`, 1)
}

func BenchmarkDecodeBudget(b *testing.B) {
	// The wait is long so that a cell that serves its workers one after another
	// is measured and not throttled.
	protocol.SetInFlight(protocol.DefaultInFlight)
	protocol.SetBodyWait(5 * time.Minute)
	b.Cleanup(func() {
		protocol.SetInFlight(0)
		protocol.SetBodyWait(protocol.DefaultBodyWait)
	})

	for _, shape := range decodeShapes() {
		for _, workers := range itemConcurrency {
			bodies := make([]string, workers)
			for worker := range workers {
				bodies[worker] = shape.body(worker)
			}

			size := len(bodies[0]) * workers
			if size > maxDecodeIterationBytes {
				continue
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
