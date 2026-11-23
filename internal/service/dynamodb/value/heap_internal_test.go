package value

import (
	"fmt"
	"math"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"
	"weak"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/stretchr/testify/require"
)

// allocSink keeps each measured allocation on the heap.
var allocSink []byte

// Test_allocBytes_coversSizeClass: for every request up to 2 KiB and every
// seventh one past it to 80 KiB, an allocation takes at most what allocBytes
// says, as the runtime's own count of allocated bytes shows.
func Test_allocBytes_coversSizeClass(t *testing.T) {
	var stats runtime.MemStats

	const (
		dense = 2 << 10
		top   = 80 << 10
		step  = 7
	)

	// took is what one allocation of n bytes added to the count, the least
	// of three tries, since another goroutine can allocate in between.
	took := func(n int) int {
		least := math.MaxInt

		for range 3 {
			runtime.ReadMemStats(&stats)
			before := stats.TotalAlloc

			allocSink = make([]byte, n)

			runtime.ReadMemStats(&stats)
			least = min(least, int(stats.TotalAlloc-before))
		}

		return least
	}

	for n := 1; n <= top; {
		require.GreaterOrEqual(t, allocBytes(n), took(n), "allocating %d bytes", n)

		if n < dense {
			n++
		} else {
			n += step
		}
	}

	allocSink = nil
}

// numbered is an item of n attributes, each a number, named from first on,
// and one string of text bytes, "text", when text is not 0.
func numbered(t *testing.T, first, n, text int) Packed {
	t.Helper()

	one := "1"
	item := make(Item, n+1)

	for i := first; i < first+n; i++ {
		item[fmt.Sprintf("a%d", i)] = api.AttributeValue{N: &one}
	}

	if text > 0 {
		s := strings.Repeat("x", text)
		item["text"] = api.AttributeValue{S: &s}
	}

	p, err := Pack(item)
	require.NoError(t, err)

	return p
}

// Test_Decoder_releaseLetsGoOfAWiderImage: a map View decodes into keeps the
// names of the last item in seen, substrings of its packed string. After a wide
// image and then a narrow one, Release lets go of the names of the wide one as
// well, past where the narrow one stopped, so its packed item, 300 KB here,
// is free to be collected once the stream trims it. The scratch is held by
// the test, not the pool, which the race detector empties at random.
func Test_Decoder_releaseLetsGoOfAWiderImage(t *testing.T) {
	scratch := new(viewScratch)

	var wide weak.Pointer[byte]

	func() {
		d := &Decoder{scratch: scratch}
		a := numbered(t, 0, 39, 300<<10)
		wide = weak.Make(unsafe.StringData(string(a)))

		require.Len(t, d.View(a, nil), 40)
		require.Len(t, d.View(numbered(t, 0, 2, 0), nil), 2)
		d.Release()
	}()

	for range 3 {
		runtime.GC() //nolint:revive // the weak pointer is read after a collection.
	}

	require.Nil(t, wide.Value(), "the packed item of the wide image is collected")
	runtime.KeepAlive(scratch)
}

// Test_Decoder_releaseDropsAScratchThatWasWide: a scratch whose map holds a
// narrow item but whose names slice has room for the wide one before it is too
// large to pool, as one whose map holds the wide one is.
func Test_Decoder_releaseDropsAScratchThatWasWide(t *testing.T) {
	scratch := new(viewScratch)
	d := &Decoder{scratch: scratch}

	require.Len(t, d.View(numbered(t, 0, 4*maxPooledView, 0), nil), 4*maxPooledView)
	require.Len(t, d.View(numbered(t, 0, 2, 0), nil), 2)
	require.Len(t, scratch.view.m, 2)
	require.True(t, scratch.view.oversized(), "room for %d names is more than a pool keeps", cap(scratch.view.seen))
}

// Test_Decoder_viewKeepsEmptyValuesAndDropsOtherNames: the names an item lacks
// are told by a value still unset, so a value that is set but empty, an empty
// list or map, a false, a null, is kept when the same item is read again, and
// each name only the item before it had is gone.
func Test_Decoder_viewKeepsEmptyValuesAndDropsOtherNames(t *testing.T) {
	yes, no, text := true, false, ""
	both := Item{
		"l": {L: []api.AttributeValue{}}, "m": {M: Item{}}, "f": {BOOL: &no}, "n": {NULL: &yes}, "s": {S: &text},
		"extra": {S: &text},
	}
	same := Item{"l": both["l"], "m": both["m"], "f": both["f"], "n": both["n"], "s": both["s"]}

	pack := func(item Item) Packed {
		p, err := Pack(item)
		require.NoError(t, err)

		return p
	}

	d := new(Decoder)
	require.Len(t, d.View(pack(both), nil), len(both))

	got := d.View(pack(same), nil)
	require.Len(t, got, len(same), "the name only the earlier item had is gone")
	require.NotContains(t, got, "extra")

	got = d.View(pack(same), nil)
	require.Len(t, got, len(same), "an empty value is not taken for a name the item lacks")
	require.Equal(t, same, got)
	d.Release()
}

// Test_Decoder_viewOfAWideImageIsLinear: View of a wide image after another,
// one name fewer, as a MODIFY record's old image follows its new one, takes
// time in proportion to the attributes, not their square: eight times the
// attributes take about eight times as long, where a pass over the names the
// map holds, for each name, would take sixty-four.
func Test_Decoder_viewOfAWideImageIsLinear(t *testing.T) {
	const small, large = 8 << 10, 64 << 10

	// took is the least of five tries of View of an image of n-1 attributes
	// after one of n, through a reused map.
	took := func(n int) time.Duration {
		newer, older := numbered(t, 0, n, 0), numbered(t, 1, n-1, 0)
		least := time.Duration(math.MaxInt64)

		for range 5 {
			d := new(Decoder)

			require.Len(t, d.View(newer, nil), n)

			start := time.Now()
			got := d.View(older, nil)
			least = min(least, time.Since(start))

			require.Len(t, got, n-1)
			require.NotContains(t, got, "a0", "the name the older image lacks is gone")
			d.Release()
		}

		return least
	}

	little, great := took(small), took(large)
	require.Less(t, float64(great)/float64(little), 28.0,
		"%d attributes took %v and %d took %v", small, little, large, great)
}
