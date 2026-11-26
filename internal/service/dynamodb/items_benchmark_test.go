package dynamodb_test

import (
	"encoding/json"
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
//	BenchmarkGetItem/payload=399KiB/concurrency=1/text=cjk
//
// At concurrency 16 one iteration is 16 simultaneous calls, each on its own
// key, so ns/op and the byte rate are per sixteen calls. The largest payload
// keeps the item just under DynamoDB's 400 KB limit.
//
// GetItem also has a text axis, at concurrency 1: the item's string is
// non-ASCII or escape-dense text, which json quotes differently from the
// ASCII of the other cells. Its value changes nothing else a call does.

var (
	itemPayloads    = []itemPayload{{"100B", 100}, {"4KiB", 4 << 10}, {"399KiB", 399 << 10}}
	itemConcurrency = []int{1, 16, 64}
)

const benchTable = "bench"

// itemText is a point on the text axis: whole units of the text, then ASCII
// to make up the size.
type itemText struct {
	name string
	unit string
}

// itemTexts are two-byte Latin among ASCII, three-byte CJK, four-byte emoji,
// json text, a third of it escaped quotes, Vietnamese, CJK and ASCII
// alternating, and pretty-printed json, escaped quotes among newlines.
var itemTexts = []itemText{
	{"latin", "caf\u00e9 na\u00efve "},
	{"cjk", "\u65e5\u672c\u8a9e\u30c6\u30ad\u30b9\u30c8"},
	{"emoji", "\U0001f389\U0001f680\u2728"},
	{"json", `{"k":"v","n":1},`},
	{"viet", "Ti\u1ebfng Vi\u1ec7t c\u00f3 nhi\u1ec1u d\u1ea5u thanh \u0111i\u1ec7u "},
	{"cjkascii", "\u65e5a\u672cb\u8a9ec"},
	{"jsonpretty", "{\n  \"name\": \"value\",\n  \"count\": 12\n}\n"},
}

// itemTextPayloads are the sizes of the text cells.
var itemTextPayloads = itemPayloads[1:]

func (t itemText) value(n int) string {
	return strings.Repeat(t.unit, n/len(t.unit)) + strings.Repeat("x", n%len(t.unit))
}

type itemPayload struct {
	name string
	size int
}

// maxIterationPutBytes is the most bytes of items one iteration of PutItem
// carries, workers times the payload. A cell over it is left out
// (tooSlowToGate), as the slowest of its runs has to be at most 8 ms. On the
// gate's runners (ten runs, bench run 45505878281 of PR 84) PutItem at 399 KiB
// and concurrency 64, 25.5 MB, took 12.3 to 14.1 ms, and 14.0 to 16.8 ms with
// persist=on; at 16, 6.4 MB, it took 3.7 to 4.0 ms, and 4.2 to 4.5 ms with
// persist=on, which the budget keeps. On the slower runner of bench run
// 45732740061 (1.65 times this family's time) the cells at 16 took 5.1 to
// 6.0 ms and, with persist=on, 5.8 to 7.1 ms: the dearest cell kept, over the
// 6.5 ms margin and under 8. GetItem reads the same bytes in at most
// 4.4 ms (399 KiB at 64), so keeps every cell (unbudgeted).
const maxIterationPutBytes = 8 << 20

// itemMatrix runs one sub-benchmark for every payload at every concurrency
// whose iteration carries at most budget bytes, each on a fresh stack holding
// the table bench, keyed by id.
func itemMatrix(b *testing.B, budget int, run func(b *testing.B, s *stack, p itemPayload, workers int)) {
	b.Helper()
	itemMatrixOn(b, newStack, budget, run)
}

// itemMatrixOn is itemMatrix on the stacks mk builds.
func itemMatrixOn(
	b *testing.B, mk func(testing.TB) *stack, budget int, run func(b *testing.B, s *stack, p itemPayload, workers int),
) {
	b.Helper()

	for _, payload := range itemPayloads {
		for _, workers := range itemConcurrency {
			if tooSlowToGate(payload.size*workers, budget) {
				continue
			}

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
	return putValue(worker, strings.Repeat("x", size))
}

// putValue is a PutItem body for the item keyed k-worker with d the string d.
func putValue(worker int, d string) string {
	value, err := json.Marshal(d)
	if err != nil {
		panic(err)
	}

	return fmt.Sprintf(`{"TableName":%q,"Item":{"id":{"S":"k-%d"},"d":{"S":%s}}}`, benchTable, worker, value)
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
	itemMatrix(b, maxIterationPutBytes, benchPut)
	b.Run(persistOn, func(b *testing.B) { itemMatrixOn(b, persistedStack, maxIterationPutBytes, benchPut) })
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

// BenchmarkPutItemConditionFailed is a PutItem whose condition fails on the
// stored item it would replace, at concurrency 1: PutItem packs the item
// before it takes the table's lock, so this is what a refused put pays for
// the item it throws away. Names carry the payload, for example
//
//	BenchmarkPutItemConditionFailed/payload=4KiB
func BenchmarkPutItemConditionFailed(b *testing.B) {
	for _, payload := range itemPayloads {
		b.Run("payload="+payload.name, func(b *testing.B) {
			s := newStack(b)
			s.ok(b, opCreate, hashTable(benchTable))
			s.ok(b, opPut, putBody(0, payload.size))

			body := strings.TrimSuffix(putBody(0, payload.size), "}") +
				`,"ConditionExpression":"attribute_not_exists(id)"}`

			b.SetBytes(int64(payload.size))
			b.ReportAllocs()

			for b.Loop() {
				got := s.call(b, usEast1, opPut, body)
				require.Equal(b, http.StatusBadRequest, got.status, got.body)
			}
		})
	}
}

// BenchmarkDeleteItem deletes one stored item a call, with ReturnValues NONE
// or ALL_OLD; the put that stores it again runs with the timer stopped. Names
// carry the axes, for example
//
//	BenchmarkDeleteItem/payload=4KiB/return=ALL_OLD
func BenchmarkDeleteItem(b *testing.B) {
	for _, payload := range itemPayloads[:2] {
		for _, returned := range []string{"NONE", "ALL_OLD"} {
			b.Run(fmt.Sprintf("payload=%s/return=%s", payload.name, returned), func(b *testing.B) {
				s := newStack(b)
				s.ok(b, opCreate, hashTable(benchTable))

				put := putBody(0, payload.size)
				body := fmt.Sprintf(`{"TableName":%q,"Key":{"id":{"S":"k-0"}},"ReturnValues":%q}`, benchTable, returned)

				b.SetBytes(int64(payload.size))
				b.ReportAllocs()

				for b.Loop() {
					b.StopTimer()
					s.ok(b, opPut, put)
					b.StartTimer()

					s.ok(b, opDeleteI, body)
				}
			})
		}
	}
}

func BenchmarkGetItem(b *testing.B) {
	itemMatrix(b, unbudgeted, func(b *testing.B, s *stack, p itemPayload, workers int) {
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

	for _, p := range itemTextPayloads {
		for _, text := range itemTexts {
			b.Run(fmt.Sprintf("payload=%s/concurrency=1/text=%s", p.name, text.name), func(b *testing.B) {
				s := newStack(b)
				s.ok(b, opCreate, hashTable(benchTable))
				s.ok(b, opPut, putValue(0, text.value(p.size)))

				body := getBody(0)

				b.SetBytes(int64(p.size))
				b.ReportAllocs()

				for b.Loop() {
					s.ok(b, opGet, body)
				}
			})
		}
	}
}
