package value

import (
	"runtime"
	"strconv"
	"strings"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_packShape_coversTheJSON: the size Pack grows its scratch buffer to
// holds the item's json, so a buffer the pool lost usually grows once, and is
// not far over it, so a large item does not take much more scratch than its
// json. Every kind of value is covered, alone and many at once, with numbers
// whose exponent or zeros ItemSize does not count. A string with control
// bytes, which the json escapes to six bytes each, is not covered: no bound
// that skips reading the string holds it, and the buffer grows again for it.
func Test_packShape_coversTheJSON(t *testing.T) {
	str := func(s string) *string { return &s }
	big := strings.Repeat("x", 64<<10)

	many := Item{}
	for i := range 2000 {
		many["a"+strconv.Itoa(i)] = api.AttributeValue{S: str("v")}
	}

	list := make([]api.AttributeValue, 1000)
	for i := range list {
		list[i] = api.AttributeValue{N: str(strconv.Itoa(i * 7919))}
	}

	falses, nulls, exponents, binaries := Item{}, Item{}, Item{}, Item{}
	for i := range 2000 {
		name := "a" + strconv.Itoa(i)
		falses[name] = api.AttributeValue{BOOL: new(false)}
		// One byte, which base64 writes as four.
		binaries[name] = api.AttributeValue{B: []byte{byte(i)}}
		nulls[name] = api.AttributeValue{NULL: new(true)}
		// One digit, so ItemSize counts two bytes for each of these.
		exponents[name] = api.AttributeValue{N: str("1e" + strconv.Itoa(100+i%25))}
	}

	zeros := make([]api.AttributeValue, 1000)
	for i := range zeros {
		zeros[i] = api.AttributeValue{N: str("0.0000000000000000000000000000000000001")}
	}

	items := map[string]Item{
		"string":     {"pk": {S: str(big)}},
		"number":     {"pk": {S: str("k")}, "n": {N: str("-1234567890.123456789e-10")}},
		"binary":     {"pk": {S: str("k")}, "b": {B: []byte(big)}},
		"attributes": many,
		"list":       {"pk": {S: str("k")}, "l": {L: list}},
		"false":      falses,
		"null":       nulls,
		"exponents":  exponents,
		"binaries":   binaries,
		"zeros":      {"pk": {S: str("k")}, "l": {L: zeros}},
		"map": {"pk": {S: str("k")}, "m": {M: map[string]api.AttributeValue{
			"x": {M: map[string]api.AttributeValue{"y": {BOOL: new(true)}, "z": {NULL: new(true)}}},
		}}},
		"sets": {
			"pk": {S: str("k")}, "ss": {SS: []string{"a", "bb", ""}}, "ns": {NS: []string{"1", "22.5", "-3e7"}},
			"bs": {BS: [][]byte{{1}, {2, 3}, []byte(big[:100])}},
		},
	}

	for name, item := range items {
		packed, err := Pack(item)
		require.NoError(t, err, name)

		h := packed.header()
		json := len(packed) - h.body

		var shape packShape
		shape.item(item)

		estimate := ItemSize(item) + shape.extra + packWrapping
		assert.GreaterOrEqual(t, estimate, json, "%s: the estimate holds the json", name)
		assert.LessOrEqual(t, estimate, json+json/2+packWrapping, "%s: the estimate is near the json", name)
	}
}

// Test_Pack_largeItemAllocatesItsSizeOnce: a large string or binary value
// packed after a collection, which empties the scratch pool, allocates one
// buffer of about the item's size, which becomes the Packed, and not a scratch
// buffer and a copy as well. A large item of many small values, whose estimate
// leaves more slack than the Packed keeps, is copied to its own size. Each
// packs to the item, whichever buffer it took. It is not parallel: another
// test's allocations would count.
func Test_Pack_largeItemAllocatesItsSizeOnce(t *testing.T) {
	big := strings.Repeat("x", 399<<10)

	many := Item{}
	for i := range 4000 {
		many["a"+strconv.Itoa(i)] = api.AttributeValue{S: new("v")}
	}

	str := Item{"pk": {S: new("k")}, "s": {S: &big}}

	items := []struct {
		name string
		item Item
		once bool
	}{
		{"string", str, true},
		{"binary", Item{"pk": {S: new("k")}, "b": {B: []byte(big)}}, true},
		{"attributes", many, false},
		// The pool holds the attributes' buffer now, so this one is copied
		// out of it.
		{"string from the pool", str, false},
	}

	for _, c := range items {
		if c.once {
			runtime.GC() //nolint:revive // a pool drops its buffers over two collections
			runtime.GC() //nolint:revive // a pool drops its buffers over two collections
		}

		var before, after runtime.MemStats

		runtime.ReadMemStats(&before)

		packed, err := Pack(c.item)

		runtime.ReadMemStats(&after)
		require.NoError(t, err, c.name)

		if c.once {
			// The runtime rounds a large allocation up to whole pages.
			const page = 8 << 10

			assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(len(packed)+len(packed)/packSlack+page),
				"%s: Pack allocated more than the item once", c.name)
		}

		assert.True(t, EqualItems(Unpack(packed), c.item), "%s: the item unpacks as it was", c.name)

		typed, err := (&api.GetItemOutput{Item: c.item}).AppendJSON(nil)
		require.NoError(t, err, c.name)
		// The encoder writes a map's members in the order it ranges them.
		assert.Len(t, packed.JSON(), len(typed)-len(itemPrefix+itemSuffix), "%s: the json is the encoder's", c.name)
		assert.Equal(t, ItemSize(c.item), packed.Size(), c.name)
	}
}
