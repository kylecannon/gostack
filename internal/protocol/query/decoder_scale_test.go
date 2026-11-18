package query_test

import (
	"fmt"
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
	scaleSmall   = 1_000
	scaleLarge   = 16_000
	scaleRuns    = 5
	scaleLimit   = 40
	scaleLinear  = scaleLarge / scaleSmall
	scaleCeiling = 5 * time.Second

	elemOne    = "L.1"
	elemTwo    = "L.2"
	elemThree  = "L.3"
	nameMember = "Name"
)

// bestDecode is the shortest of scaleRuns decodes of a batch of n entries, so a
// pause of the scheduler or the collector does not decide the ratio.
func bestDecode(t *testing.T, vals url.Values, n int) time.Duration {
	t.Helper()

	best := time.Duration(1<<63 - 1)

	for range scaleRuns {
		start := time.Now()
		entries, attributes := decodeBatch(vals)
		took := time.Since(start)

		require.Equal(t, n, entries)
		require.Equal(t, n, attributes)

		best = min(best, took)
	}

	return best
}

// Test_Decoder_BatchScalesWithEntries pins the decode of a batch to linear time.
// A key below another used to be found by scanning every key of the form, so
// decoding n entries took time in n squared: about 256 times as long for 16
// times the entries. Linear is 16 times, and the bound leaves room for n log n
// and for timing noise. It runs under the race detector, which slows both sizes
// alike.
func Test_Decoder_BatchScalesWithEntries(t *testing.T) {
	small := bestDecode(t, batchForm(scaleSmall), scaleSmall)
	large := bestDecode(t, batchForm(scaleLarge), scaleLarge)

	ratio := float64(large) / float64(small)
	t.Logf("%d entries took %v, %d took %v: ratio %.1f (linear is %d)",
		scaleSmall, small, scaleLarge, large, ratio, scaleLinear)

	assert.Less(t, ratio, float64(scaleLimit))
	assert.Less(t, large, scaleCeiling)
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
