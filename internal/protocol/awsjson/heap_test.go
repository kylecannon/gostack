package awsjson_test

import (
	"encoding/base64"
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// heapNoise is what the heap measured around one decode may move by for
// reasons of its own: the runtime's and the test framework's allocations.
const heapNoise = 64 << 10

// liveAfterDecode decodes body into a T as a handler does and returns how much
// more of the heap is live with the decoded value than before it, each figure
// read after two collections.
func liveAfterDecode[T any](t testing.TB, body []byte) int64 {
	t.Helper()

	var before, after runtime.MemStats

	runtime.GC() //nolint:revive // what is live is measured after collections
	runtime.GC() //nolint:revive // see above
	runtime.ReadMemStats(&before)

	v, _, err := awsjson.DecodeWithin[T](body, math.MaxInt64, nil)
	require.Nil(t, err)

	runtime.GC() //nolint:revive // see above
	runtime.GC() //nolint:revive // see above
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(v)

	return int64(after.HeapAlloc) - int64(before.HeapAlloc)
}

// batchOf is a BatchWriteItem of items, each an item of one attribute whose
// value is a list of count copies of element.
func batchOf(items, count int, element string) []byte {
	var b strings.Builder

	b.WriteString(`{"RequestItems":{"t":[`)

	for i := range items {
		if i > 0 {
			b.WriteByte(',')
		}

		fmt.Fprintf(&b, `{"PutRequest":{"Item":{"id":{"S":"%d"},"v":{"L":[`, i)

		for j := range count {
			if j > 0 {
				b.WriteByte(',')
			}

			b.WriteString(element)
		}

		b.WriteString(`]}}}}`)
	}

	b.WriteString(`]}}`)

	return []byte(b.String())
}

// wideItem is a BatchWriteItem of the given number of items, each with the
// given number of attributes.
func wideItem(items, attributes int, value string) []byte {
	var b strings.Builder

	b.WriteString(`{"RequestItems":{"t":[`)

	for i := range items {
		if i > 0 {
			b.WriteByte(',')
		}

		fmt.Fprintf(&b, `{"PutRequest":{"Item":{"id":{"S":"%d"}`, i)

		for j := range attributes {
			fmt.Fprintf(&b, `,"attr%d":%s`, j, value)
		}

		b.WriteString(`}}}`)
	}

	b.WriteString(`]}}`)

	return []byte(b.String())
}

// chain is n one-entry maps (or lists) nested around leaf.
func chain(n int, open, closing, leaf string) string {
	return strings.Repeat(open, n) + leaf + strings.Repeat(closing, n)
}

// zoo is the shapes SQ-457 and SQ-449 measured, a sixteenth of their size: the
// densest lists of each kind DynamoDB's limits allow, long and short, nested
// to the depth limit, wide items and one large map.
func zoo() map[string][]byte {
	strs := `{"SS":[` + strings.TrimSuffix(strings.Repeat(`"a",`, 10_000), ",") + `]}`

	return map[string][]byte{
		"n25x64000":     batchOf(25, 4000, `{"N":"1"}`),
		"s0_25x74000":   batchOf(25, 4600, `{"S":""}`),
		"m0_25x74000":   batchOf(25, 4600, `{"M":{}}`),
		"l0":            batchOf(25, 4600, `{"L":[]}`),
		"null":          batchOf(25, 4000, `{"NULL":true}`),
		"bool":          batchOf(25, 4000, `{"BOOL":false}`),
		"b1":            batchOf(25, 4000, `{"B":"AA=="}`),
		"lm1_25x30000":  batchOf(25, 1900, `{"M":{"a":{"N":"1"}}}`),
		"lm1e_25x30000": batchOf(25, 1900, `{"M":{"a":{"M":{}}}}`),
		"lm1s_25x31900": batchOf(25, 2000, `{"M":{"a":{"S":""}}}`),
		"lm2":           batchOf(25, 1200, `{"M":{"a":{"N":"1"},"b":{"N":"2"}}}`),
		"lm9": batchOf(10, 300, `{"M":{"k":{"N":"1"},"b":{"N":"1"},"c":{"N":"1"},"d":{"N":"1"},`+
			`"e":{"N":"1"},"f":{"N":"1"},"g":{"N":"1"},"h":{"N":"1"},"i":{"N":"1"}}}`),
		"mchain31_25x1740": batchOf(25, 110, chain(31, `{"M":{"a":`, `}}`, `{"N":"1"}`)),
		"lchain31_25x2600": batchOf(25, 160, chain(31, `{"L":[`, `]}`, `{"N":"1"}`)),
		"attrs_25x39000":   wideItem(25, 2400, `{"N":"1"}`),
		"bigmap_900k":      wideItem(1, 56_000, `{"S":"x"}`),
		"ss_25x160000":     batchOf(25, 1, strs),
		"ns":               batchOf(25, 1, strings.ReplaceAll(strs, `"SS"`, `"NS"`)),
		"strings_25x13200": batchOf(25, 800, `{"S":"`+strings.Repeat("s", 40)+`"}`),
		"few_long_lists":   batchOf(1, 100_000, `{"N":"1"}`),
		"onelist_1000k":    batchOf(1, 60_000, `{"BOOL":true}`),
	}
}

// Test_heapEstimate_coversLiveHeap holds the heap the walk bounds a DynamoDB
// body to keep (awsjson.HeapEstimate) to the heap the decoded value keeps
// live, measured, for every shape of the zoo and for random nests of every
// kind of AttributeValue, and logs the worst ratio of the estimate to it.
func Test_heapEstimate_coversLiveHeap(t *testing.T) {
	bodies := zoo()
	r := rand.New(rand.NewPCG(1, 2))

	for i := range 24 {
		bodies[fmt.Sprintf("random_%02d", i)] = randomBatch(r, 256<<10+r.IntN(768<<10))
	}

	worst, best := math.Inf(1), 0.0

	for name, body := range bodies {
		live := liveAfterDecode[api.BatchWriteItemInput](t, body)
		estimate, peak := awsjson.HeapEstimate[api.BatchWriteItemInput](body)

		assert.LessOrEqual(t, live, estimate+heapNoise, "%s: %d bytes of body", name, len(body))

		ratio := float64(estimate) / float64(max(live, 1))
		worst, best = min(worst, ratio), max(best, ratio)

		t.Logf("%-18s body %9d live %10d estimate %10d (%.2f) peak %9d", name, len(body), live, estimate, ratio, peak)
	}

	t.Logf("estimate over live heap: %.2f at the closest, %.2f at the furthest", worst, best)
}

// Fuzz_heapEstimate_coversLiveHeap is the property for random nests from a
// fuzzed seed and size.
func Fuzz_heapEstimate_coversLiveHeap(f *testing.F) {
	f.Add(uint64(1), uint32(64<<10))
	f.Add(uint64(7), uint32(512<<10))

	f.Fuzz(func(t *testing.T, seed uint64, size uint32) {
		r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
		body := randomBatch(r, int(size%(1<<20)))

		live := liveAfterDecode[api.BatchWriteItemInput](t, body)
		estimate, _ := awsjson.HeapEstimate[api.BatchWriteItemInput](body)

		require.LessOrEqual(t, live, estimate+heapNoise, "seed %d, %d bytes of body", seed, len(body))
	})
}

// randomBatch is a BatchWriteItem of random items of about size bytes in all.
func randomBatch(r *rand.Rand, size int) []byte {
	var b strings.Builder

	b.WriteString(`{"RequestItems":{"t":[`)

	for i := 0; i == 0 || b.Len() < size; i++ {
		if i > 0 {
			b.WriteByte(',')
		}

		fmt.Fprintf(&b, `{"PutRequest":{"Item":{"id":{"S":"%d"}`, i)

		for j := range r.IntN(8) {
			fmt.Fprintf(&b, `,"a%d":`, j)
			randomValue(r, &b, r.IntN(33), size)
		}

		b.WriteString(`}}}`)
	}

	b.WriteString(`]}}`)

	return []byte(b.String())
}

// randomValue writes a random AttributeValue nested at most depth deep, its
// lists and maps of a width drawn to reach every layout the runtime has for
// them: empty, one group, tables, and long arrays, until b holds size bytes.
func randomValue(r *rand.Rand, b *strings.Builder, depth, size int) {
	kinds := 10
	if depth == 0 {
		kinds = 8
	}

	switch r.IntN(kinds) {
	case 0:
		fmt.Fprintf(b, `{"N":"%d"}`, r.Int64())
	case 1:
		fmt.Fprintf(b, `{"S":%q}`, strings.Repeat("s", r.IntN(80)))
	case 2:
		fmt.Fprintf(b, `{"B":%q}`, base64.StdEncoding.EncodeToString(make([]byte, r.IntN(60))))
	case 3:
		b.WriteString(`{"NULL":true}`)
	case 4:
		b.WriteString(`{"BOOL":false}`)
	case 5:
		set(r, b, "SS", `"%d"`)
	case 6:
		set(r, b, "NS", `"%d"`)
	case 7:
		set(r, b, "BS", `"AA%02d"`)
	case 8:
		b.WriteString(`{"M":{`)

		for i := range width(r) {
			if b.Len() > size {
				break
			}

			if i > 0 {
				b.WriteByte(',')
			}

			fmt.Fprintf(b, `"k%d":`, i)
			randomValue(r, b, depth-1, size)
		}

		b.WriteString(`}}`)
	default:
		b.WriteString(`{"L":[`)

		for i := range width(r) {
			if b.Len() > size {
				break
			}

			if i > 0 {
				b.WriteByte(',')
			}

			randomValue(r, b, depth-1, size)
		}

		b.WriteString(`]}`)
	}
}

// set writes a set of a random width whose members are format of their index.
func set(r *rand.Rand, b *strings.Builder, kind, format string) {
	fmt.Fprintf(b, `{%q:[`, kind)

	for i := range max(width(r), 1) {
		if i > 0 {
			b.WriteByte(',')
		}

		fmt.Fprintf(b, format, i%100)
	}

	b.WriteString(`]}`)
}

// width is how many members a random list or map has: mostly a few, sometimes
// one group's eight and one past it, sometimes thousands.
func width(r *rand.Rand) int {
	switch r.IntN(10) {
	case 0:
		return 0
	case 1:
		return 8 + r.IntN(2)
	case 2:
		return r.IntN(5000)
	default:
		return 1 + r.IntN(4)
	}
}
