package value_test

import (
	"encoding/base64"
	"encoding/json/jsontext"
	"maps"
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

// The typed item encoder the snapshot wrote items with before they were
// packed, kept as the oracle for value.Orderer: a snapshot written from packed
// items must be the bytes this wrote.

// snapshotItem appends item as a json object, its names in byte order, so the
// same item always gives the same bytes.
func snapshotItem(dst []byte, item value.Item) []byte {
	dst = append(dst, '{')

	for i, name := range slices.Sorted(maps.Keys(item)) {
		if i > 0 {
			dst = append(dst, ',')
		}

		v := item[name]
		dst = awsjson.AppendQuote(dst, name)
		dst = append(dst, ':')
		dst = snapshotValue(dst, &v)
	}

	return append(dst, '}')
}

// snapshotValue appends v as a json object holding the one member that is set.
func snapshotValue(dst []byte, v *api.AttributeValue) []byte {
	switch {
	case v.S != nil:
		return append(awsjson.AppendQuote(append(dst, `{"S":`...), *v.S), '}')
	case v.N != nil:
		return append(awsjson.AppendQuote(append(dst, `{"N":`...), *v.N), '}')
	case v.B != nil:
		return append(snapshotBytes(append(dst, `{"B":`...), v.B), '}')
	case v.NULL != nil:
		return append(strconv.AppendBool(append(dst, `{"NULL":`...), *v.NULL), '}')
	case v.BOOL != nil:
		return append(strconv.AppendBool(append(dst, `{"BOOL":`...), *v.BOOL), '}')
	case v.SS != nil:
		return append(snapshotStrings(append(dst, `{"SS":`...), v.SS), '}')
	case v.NS != nil:
		return append(snapshotStrings(append(dst, `{"NS":`...), v.NS), '}')
	case v.BS != nil:
		return append(snapshotBinaries(append(dst, `{"BS":`...), v.BS), '}')
	case v.M != nil:
		return append(snapshotItem(append(dst, `{"M":`...), v.M), '}')
	default:
		return append(snapshotList(append(dst, `{"L":`...), v.L), '}')
	}
}

func snapshotStrings(dst []byte, list []string) []byte {
	dst = append(dst, '[')

	for i, s := range list {
		if i > 0 {
			dst = append(dst, ',')
		}

		dst = awsjson.AppendQuote(dst, s)
	}

	return append(dst, ']')
}

func snapshotBytes(dst, b []byte) []byte {
	dst = append(dst, '"')
	dst = base64.StdEncoding.AppendEncode(dst, b)

	return append(dst, '"')
}

func snapshotBinaries(dst []byte, list [][]byte) []byte {
	dst = append(dst, '[')

	for i, b := range list {
		if i > 0 {
			dst = append(dst, ',')
		}

		dst = snapshotBytes(dst, b)
	}

	return append(dst, ']')
}

func snapshotList(dst []byte, list []api.AttributeValue) []byte {
	dst = append(dst, '[')

	for i := range list {
		if i > 0 {
			dst = append(dst, ',')
		}

		dst = snapshotValue(dst, &list[i])
	}

	return append(dst, ']')
}

// header is the header of p: its size, scalar count and attribute count.
func header(p value.Packed) string {
	return string(p[:len(p)-len(p.JSON())])
}

func s(v string) api.AttributeValue { return api.AttributeValue{S: &v} }
func n(v string) api.AttributeValue { return api.AttributeValue{N: &v} }

// packerItems are items of every attribute type, with names and strings the
// json must escape, maps whose names sort apart once decoded and raw, and maps
// inside lists and maps.
func packerItems() []value.Item {
	return []value.Item{
		{},
		{
			"id": s("scalars"), "n": n("-1500"), "b": {B: []byte{0, 1, 2, 0xff}}, "eb": {B: []byte{}},
			"t": {BOOL: new(true)}, "f": {BOOL: new(false)}, "z": {NULL: new(true)}, "e": s(""),
		},
		{
			"ss": {SS: []string{"b", "a", "é"}}, "ns": {NS: []string{"10", "2.5", "-3"}},
			"bs": {BS: [][]byte{{2}, {1}, {}}},
		},
		{
			"escapes \"quoted\" \\ \t\n\u0001 😀 </script>": s("tab\tnl\n" + string(rune(0x2028)) + " < > &"),
			"a\"": s("1"), "a#": s("2"), "a": s("3"), "a\\": s("4"), "\u0001": n("5"), "é": {BOOL: new(true)},
		},
		{
			"k9": s("1"), "k10": s("2"), "k1": s("3"), "K": {BS: [][]byte{{1}, {2}}}, "k": {NS: []string{"2", "1"}},
		},
		{
			"z": {M: map[string]api.AttributeValue{
				"y": {M: map[string]api.AttributeValue{
					"x": s("1"),
					"w": {L: []api.AttributeValue{{M: map[string]api.AttributeValue{"v": n("1"), "u": n("2")}}, s("t")}},
				}},
				"s": {L: []api.AttributeValue{}}, "r": {M: map[string]api.AttributeValue{}},
			}},
			"q": {L: []api.AttributeValue{{L: []api.AttributeValue{{M: map[string]api.AttributeValue{
				"p": {NULL: new(true)}, "o": {SS: []string{"n", "m"}},
			}}}}}},
		},
		deepItem(value.MaxDepth),
	}
}

// deepItem is an item whose attribute is depth maps deep, counting itself.
func deepItem(depth int) value.Item {
	v := s("x")
	for range depth - 1 {
		v = api.AttributeValue{M: map[string]api.AttributeValue{"a": v}}
	}

	return value.Item{"a": v}
}

// randomItems are the items of packerItems and random ones that DynamoDB
// accepts.
func randomItems(count int) []value.Item {
	items := packerItems()
	r := rand.New(rand.NewPCG(3, 4))

	for len(items) < count {
		item := randomPackItem(r)
		if value.ValidateItem(item) == nil {
			items = append(items, item)
		}
	}

	return items
}

// Test_JSONPacker_packsAsPack: JSONPacker, given the json Pack wrote for an
// item, packs the same bytes: the header (size, scalar count and attribute
// count) and the json as it was read.
func Test_JSONPacker_packsAsPack(t *testing.T) {
	t.Parallel()

	var k value.JSONPacker

	for _, item := range randomItems(500) {
		want, err := value.Pack(item)
		require.NoError(t, err)

		got, ok := k.Pack([]byte(want.JSON()))
		require.True(t, ok, "refused %s", want.JSON())
		require.Equal(t, header(want), header(got), want.JSON())
		require.Equal(t, want.JSON(), got.JSON())
		require.Equal(t, string(want), string(got), want.JSON())
		require.True(t, value.EqualItems(item, value.Unpack(got)), want.JSON())

		// The packed item is its own: it shares no byte of what it was read from.
		raw := []byte(want.JSON())
		got, ok = k.Pack(raw)
		require.True(t, ok)

		for i := range raw {
			raw[i] = 0
		}

		require.Equal(t, string(want), string(got))
	}

	_, ok := k.Pack(nil)
	require.False(t, ok)
}

// Test_JSONPacker_leavesWhatPackWouldRewrite: json that is valid but that Pack
// would write another way, and an item DynamoDB refuses, is left to the typed
// path.
func Test_JSONPacker_leavesWhatPackWouldRewrite(t *testing.T) {
	t.Parallel()

	var k value.JSONPacker

	for _, text := range []string{
		`{ "a":{"S":"x"}}`, `{"a":{"S":"x"} }`, `{"` + `\` + `u0061":{"S":"x"}}`, `{"a":{"S":"` + `\` + `u0078"}}`,
		`{"":{"S":"x"}}`, `{"a":{}}`, `{"a":{"S":"x","N":"1"}}`, `{"a":{"X":"x"}}`,
		`{"a":{"N":"01"}}`, `{"a":{"N":"1E2"}}`, `{"a":{"N":"-0"}}`, `{"a":{"B":"AQ"}}`,
		`{"a":{"SS":[]}}`, `{"a":{"SS":["x","x"]}}`, `{"a":{"NULL":false}}`,
	} {
		_, ok := k.Pack([]byte(text))
		require.False(t, ok, "%q", text)
	}

	deeper, err := value.Pack(deepItem(value.MaxDepth + 1))
	require.NoError(t, err)

	_, ok := k.Pack([]byte(deeper.JSON()))
	require.False(t, ok, "an item nested deeper than DynamoDB allows")
}

// FuzzJSONPacker: whatever json JSONPacker takes, it takes as Pack packs the
// item it reads: an item ValidateItem accepts, with the same header and the
// same json once the maps are in order.
func FuzzJSONPacker(f *testing.F) {
	for _, item := range packerItems() {
		p, err := value.Pack(item)
		if err != nil {
			f.Fatal(err)
		}

		f.Add([]byte(p.JSON()))
	}

	f.Add([]byte(`{"a":{"S":"x"}}`))
	f.Add([]byte(`{"a":{"N":"01"}}`))

	f.Fuzz(func(t *testing.T, raw []byte) {
		// The snapshot's reader has checked the json is valid, and that no
		// object repeats a name, before it gives an item to the packer.
		if !jsontext.Value(raw).IsValid(jsontext.AllowInvalidUTF8(true)) {
			return
		}

		var k value.JSONPacker

		got, ok := k.Pack(raw)
		if !ok {
			return
		}

		item := value.Unpack(got)
		require.NoError(t, value.ValidateItem(item), "packed what ValidateItem refuses: %s", raw)

		want, err := value.Pack(item)
		require.NoError(t, err)
		require.Equal(t, header(want), header(got), string(raw))

		var o value.Orderer

		require.Equal(t, string(o.Append(nil, want)), string(o.Append(nil, got)), string(raw))
	})
}

// Test_Orderer_writesTheSnapshotsItems: Orderer gives, for every item, the
// bytes the snapshot wrote the item as before items were packed, whatever
// order the encoder put its names in.
func Test_Orderer_writesTheSnapshotsItems(t *testing.T) {
	t.Parallel()

	var o value.Orderer

	for _, item := range randomItems(300) {
		want := snapshotItem(nil, item)

		// Map order differs between packs of the same item.
		for range 10 {
			p, err := value.Pack(item)
			require.NoError(t, err)
			require.Equal(t, string(want), string(o.Append(nil, p)))
		}
	}

	require.Empty(t, o.Append(nil, ""))
}

// FuzzOrderer: Orderer gives the snapshot's bytes for an item drawn from a
// seed, and appends to what is there.
func FuzzOrderer(f *testing.F) {
	f.Add(uint64(1), uint64(2))
	f.Add(uint64(0), uint64(0))

	f.Fuzz(func(t *testing.T, a, b uint64) {
		item := randomPackItem(rand.New(rand.NewPCG(a, b)))
		if value.ValidateItem(item) != nil {
			return
		}

		p, err := value.Pack(item)
		require.NoError(t, err)

		var o value.Orderer

		want := snapshotItem([]byte("x"), item)
		require.Equal(t, string(want), string(o.Append([]byte("x"), p)))
	})
}
