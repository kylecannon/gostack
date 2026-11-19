package query_test

import (
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/protocol/query"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// scaleRuns is how many times each size is decoded. The cost of a size is the
	// shortest run, so the figure is what the decode costs when nothing else
	// wants the CPU, and enough runs leave one that nothing interrupted.
	scaleRuns = 20
	// scaleExponent is the most the decode may grow faster than its input.
	// Linear is 1 and the sort of the keys adds a little; the scan this test
	// exists to catch is 2.
	scaleExponent = 1.5
	// scaleCeiling stops one run that has already blown up, so a quadratic decode
	// fails in seconds and not in minutes.
	scaleCeiling = 2 * time.Second

	elemOne    = "L.1"
	elemTwo    = "L.2"
	elemThree  = "L.3"
	nameMember = "Name"
)

// scaleSizes are the batch sizes decoded. They are small, so a run is short
// enough to finish inside one slice of the scheduler on a busy machine, and
// there are five, so one size that happened to be disturbed moves the fitted
// exponent little.
var scaleSizes = []int{500, 1_000, 2_000, 4_000, 8_000}

// bestDecode is the shortest of scaleRuns decodes of a batch of n entries, so a
// pause of the scheduler or the collector does not decide the figure.
func bestDecode(t *testing.T, n int) time.Duration {
	t.Helper()

	vals := batchForm(n)
	best := time.Duration(1<<63 - 1)

	for range scaleRuns {
		start := time.Now()
		entries, attributes := decodeBatch(vals)
		took := time.Since(start)

		require.Equal(t, n, entries)
		require.Equal(t, n, attributes)
		require.Less(t, took, scaleCeiling, "one decode of %d entries took %v", n, took)

		best = min(best, took)
	}

	return best
}

// growthExponent is the slope of the least-squares line through the points
// (log size, log cost): the k in cost = c * size^k.
func growthExponent(sizes []int, costs []time.Duration) float64 {
	var sumX, sumY, sumXY, sumXX float64

	for i, n := range sizes {
		x, y := math.Log(float64(n)), math.Log(float64(costs[i]))
		sumX += x
		sumY += y
		sumXY += x * y
		sumXX += x * x
	}

	count := float64(len(sizes))

	return (count*sumXY - sumX*sumY) / (count*sumXX - sumX*sumX)
}

// Test_Decoder_BatchScalesWithEntries pins the decode of a batch to near-linear
// time. A key below another used to be found by scanning every key of the form,
// so decoding n entries took time in n squared. The test decodes five sizes,
// fits the exponent of the cost against the size and requires it under
// scaleExponent: a quadratic step comes out near 2, a decode over a sorted index
// near 1.1. It replaced one ratio of two wall-clock figures (1,000 and 16,000
// entries, limit 40), which failed on a loaded machine (CI run 44592554988
// measured 40.4; a busy host measured 55.6 and 69.5; alone it measures 21 to
// 25) because a busy scheduler disturbs the one long run the ratio rests on.
// The fit does not rest on one figure, and its runs are short. The race
// detector slows every size alike, so the test needs no skip under it.
func Test_Decoder_BatchScalesWithEntries(t *testing.T) {
	costs := make([]time.Duration, len(scaleSizes))
	for i, n := range scaleSizes {
		costs[i] = bestDecode(t, n)
	}

	exponent := growthExponent(scaleSizes, costs)
	t.Logf("%v entries took %v: exponent %.2f (linear is 1, quadratic is 2)", scaleSizes, costs, exponent)

	assert.Less(t, exponent, scaleExponent)
}

// scanHas is the answer Has gives: the key itself or any key below it.
func scanHas(vals url.Values, key string) bool {
	if _, ok := vals[key]; ok {
		return true
	}

	for k := range vals {
		if strings.HasPrefix(k, key+".") {
			return true
		}
	}

	return false
}

// Test_Decoder_HasAgreesWithScan checks the sorted index against the scan it
// replaced, over keys that sit next to each other in sort order: a longer name,
// a dot, a digit, an uppercase and a lowercase letter, a dash.
func Test_Decoder_HasAgreesWithScan(t *testing.T) {
	t.Parallel()

	vals := values(t, "L.1.Name=a&L.1=b&L.10.Name=c&L.1a=d&L.2.Name.X=e&L-1=f&L=g&LA.1=h&M.1.Name=i&N.%2E=j")
	vals.Set("", "empty")

	probes := []string{
		"", "L", elemOne, "L.1.Name", "L.10", "L.10.Name", "L.1a", elemTwo, "L.2.Name", "L.2.Name.X",
		"L.2.Name.Y", elemThree, "L-1", "L-", "LA", "LA.1", "La", "M", "M.1", "M.1.Name", "M.1.Name.Z", "N", "N.", "N..",
		"Z", "A", "L.", "L.1.", "L.1.Nam",
	}

	for _, key := range probes {
		d := query.NewDecoder(vals)
		assert.Equal(t, scanHas(vals, key), d.Has(key), "key %q", key)
	}

	// One Decoder answers every probe, in any order, from one index.
	d := query.NewDecoder(vals)
	for _, key := range slices.Backward(probes) {
		assert.Equal(t, scanHas(vals, key), d.Has(key), "key %q", key)
	}
}

func Test_Decoder_HasOfEmptyForm(t *testing.T) {
	t.Parallel()

	d := query.NewDecoder(url.Values{})

	assert.False(t, d.Has("A"))
	assert.False(t, d.Has("A"), "the second lookup uses the index that is empty")
	assert.Nil(t, d.List("L", &query.ListSpec{Names: []string{"L"}, Complex: true}))
}

// Test_Decoder_ComplexListOrderAndGaps pins what a list of structures does with
// numbers out of order, a gap, and a key that repeats: the elements follow the
// element number, not the sort order of the keys or the order of the form; the
// first gap ends the list; and a repeated key counts once, with its first value.
func Test_Decoder_ComplexListOrderAndGaps(t *testing.T) {
	t.Parallel()

	spec := query.ListSpec{Names: []string{"L"}, Flattened: true, Complex: true}

	t.Run("numbers follow the element number, not the sort order", func(t *testing.T) {
		t.Parallel()

		var raw []string
		for i := 12; i >= 1; i-- {
			raw = append(raw, "L."+strconv.Itoa(i)+".Name=n"+strconv.Itoa(i))
		}

		d := query.NewDecoder(values(t, strings.Join(raw, "&")))

		var want []string
		for i := 1; i <= 12; i++ {
			want = append(want, "L."+strconv.Itoa(i))
		}

		got := d.List("", &spec)
		require.Equal(t, want, got)

		for i, k := range got {
			assert.Equal(t, "n"+strconv.Itoa(i+1), *d.String(query.Key(k, "Name")))
		}
	})

	t.Run("the first gap ends the list", func(t *testing.T) {
		t.Parallel()

		d := query.NewDecoder(values(t, "L.1.Name=a&L.2.Name=b&L.4.Name=d&L.5.Name=e"))

		assert.Equal(t, []string{elemOne, elemTwo}, d.List("", &spec))
	})

	t.Run("a repeated key is one element and the first value wins", func(t *testing.T) {
		t.Parallel()

		d := query.NewDecoder(values(t, "L.1.Name=a&L.1.Name=z&L.2.Name=b&L.2.Name=y"))

		got := d.List("", &spec)
		require.Equal(t, []string{elemOne, elemTwo}, got)
		assert.Equal(t, "a", *d.String("L.1.Name"))
		assert.Equal(t, "b", *d.String("L.2.Name"))
	})

	t.Run("an element is the key itself or any key below it", func(t *testing.T) {
		t.Parallel()

		d := query.NewDecoder(values(t, "L.1=plain&L.2.Name=b&L.3.A.B=c"))

		assert.Equal(t, []string{elemOne, elemTwo, elemThree}, d.List("", &spec))
	})
}

// Test_Decoder_ComplexMapGaps pins the same for a map whose values are
// structures: entry numbers go in order, an entry needs its key and a value
// that is a key or has keys below it, and the first entry that lacks one ends
// the map.
func Test_Decoder_ComplexMapGaps(t *testing.T) {
	t.Parallel()

	spec := query.MapSpec{Names: []string{"M"}, Key: nameMember, Value: valueMember, Flattened: true, ComplexValue: true}

	var raw []string
	for i := 11; i >= 1; i-- {
		n := strconv.Itoa(i)
		raw = append(raw, "M."+n+".Name=k"+n, "M."+n+".Value.Type=String")
	}

	d := query.NewDecoder(values(t, strings.Join(raw, "&")))
	got := d.Map("", &spec)

	require.Len(t, got, 11)

	for i, e := range got {
		n := strconv.Itoa(i + 1)
		assert.Equal(t, query.Entry{Key: "M." + n + ".Name", Value: "M." + n + ".Value"}, e)
		assert.Equal(t, "k"+n, *d.String(e.Key))
	}

	gap := query.NewDecoder(values(t, "M.1.Name=a&M.1.Value.T=x&M.2.Name=b&M.3.Name=c&M.3.Value.T=z"))
	assert.Len(t, gap.Map("", &spec), 1, "entry 2 has no value, so the map ends before it")
}

// Test_Decoder_NestedComplexList is the shape that made Has hot twice: a list
// of structures whose members are lists of structures.
func Test_Decoder_NestedComplexList(t *testing.T) {
	t.Parallel()

	const outer, inner = 50, 20

	vals := url.Values{}

	for i := 1; i <= outer; i++ {
		for j := 1; j <= inner; j++ {
			vals.Set(fmt.Sprintf("O.%d.I.%d.V", i, j), "v")
		}
	}

	d := query.NewDecoder(vals)
	outerSpec := query.ListSpec{Names: []string{"O"}, Flattened: true, Complex: true}
	innerSpec := query.ListSpec{Names: []string{"I"}, Flattened: true, Complex: true}

	got := 0

	for _, o := range d.List("", &outerSpec) {
		keys := d.List(o, &innerSpec)
		require.Len(t, keys, inner)

		got += len(keys)
	}

	assert.Equal(t, outer*inner, got)
}
