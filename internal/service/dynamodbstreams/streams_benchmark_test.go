package dynamodbstreams_test

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// The stream benchmark matrix (docs/standards.md, "Benchmarks"), through the
// real gateway, in process. Sub-benchmark names carry the axes, for example
//
//	BenchmarkPutItem_stream/view=NEW_AND_OLD_IMAGES/payload=4KiB/concurrency=16
//	BenchmarkGetRecords/limit=100/payload=4KiB
//
// BenchmarkPutItem_stream is write throughput with the stream off (view=off)
// and on for each view type: what recording costs a write. Every put changes
// its item, so every one leaves a record. At concurrency 16 one iteration is
// 16 simultaneous puts, each on its own key. BenchmarkGetRecords reads a page
// of limit records from a shard that holds them.

var (
	streamViews       = []string{"off", "KEYS_ONLY", "NEW_IMAGE", "OLD_IMAGE", "NEW_AND_OLD_IMAGES"}
	streamPayloads    = []benchPayload{{"100B", 100}, {"4KiB", 4 << 10}}
	streamConcurrency = []int{1, 16}
	recordLimits      = []int{1, 100, 1000}
)

type benchPayload struct {
	name string
	size int
}

func BenchmarkPutItem_stream(b *testing.B) {
	for _, view := range streamViews {
		for _, payload := range streamPayloads {
			for _, workers := range streamConcurrency {
				b.Run(fmt.Sprintf("view=%s/payload=%s/concurrency=%d", view, payload.name, workers), func(b *testing.B) {
					s := newStack(b)
					if view == "off" {
						s.ddbOK(b, "CreateTable", tableBody("bench", ""))
					} else {
						s.createStream(b, "bench", view)
					}

					benchPuts(b, s, payload.size, workers)
				})
			}
		}
	}
}

// benchPuts runs b.N rounds of workers simultaneous puts of size bytes.
func benchPuts(b *testing.B, s *stack, size, workers int) {
	b.Helper()

	data := strings.Repeat("x", size)

	var (
		round  int
		failed atomic.Int32
	)

	b.SetBytes(int64(size * workers))
	b.ReportAllocs()

	for b.Loop() {
		round++

		var wg sync.WaitGroup

		for w := range workers {
			wg.Go(func() {
				got := s.call(b, ddbTarget, "PutItem", fmt.Sprintf(
					`{"TableName":"bench","Item":{"id":{"S":"k-%d"},"r":{"N":"%d"},"d":{"S":%q}}}`, w, round, data))
				if got.status != http.StatusOK {
					failed.Add(1)
				}
			})
		}

		wg.Wait()
	}

	require.Zero(b, failed.Load())
}

func BenchmarkGetRecords(b *testing.B) {
	for _, limit := range recordLimits {
		for _, payload := range streamPayloads {
			b.Run(fmt.Sprintf("limit=%d/payload=%s", limit, payload.name), func(b *testing.B) {
				s := newStack(b)
				arn, shard := s.createStream(b, "bench", "NEW_AND_OLD_IMAGES")

				data := strings.Repeat("x", payload.size)
				for i := range limit {
					s.ddbOK(b, "PutItem", fmt.Sprintf(`{"TableName":"bench","Item":{"id":{"S":"k-%d"},"d":{"S":%q}}}`, i, data))
				}

				body := encode(b, map[string]any{keyShardIterator: s.iterator(b, arn, shard, "TRIM_HORIZON"), keyLimit: limit})

				// A page stops at 1 MB, so the byte rate is the response's own.
				first := s.call(b, streamsTarget, opGetRecords, body)
				require.Equal(b, http.StatusOK, first.status, first.body)

				b.SetBytes(int64(len(first.body)))
				b.ReportAllocs()

				for b.Loop() {
					if got := s.call(b, streamsTarget, opGetRecords, body); got.status != http.StatusOK {
						b.Fatal(got.body)
					}
				}
			})
		}
	}
}
