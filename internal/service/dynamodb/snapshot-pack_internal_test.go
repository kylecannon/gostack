package dynamodb

import (
	"encoding/base64"
	"encoding/json/jsontext"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

// The typed item encoder the snapshot wrote items with before they were
// packed, kept as the oracle for value.Orderer: a snapshot written from packed
// items must be the bytes this wrote.

// appendItem appends item as a json object, its names in byte order, so the
// same item always gives the same bytes.
func appendItem(dst []byte, item value.Item) []byte {
	names := slices.Sorted(maps.Keys(item))

	dst = append(dst, '{')

	for i, name := range names {
		if i > 0 {
			dst = append(dst, ',')
		}

		dst = awsjson.AppendQuote(dst, name)
		dst = append(dst, ':')
		v := item[name]
		dst = appendValue(dst, &v)
	}

	return append(dst, '}')
}

// appendValue appends v as a json object holding each member that is set, in
// a fixed order. A stored value has exactly one.
func appendValue(dst []byte, v *api.AttributeValue) []byte {
	dst = append(dst, '{')
	start := len(dst)
	dst = appendScalars(dst, start, v)
	dst = appendCollections(dst, start, v)

	return append(dst, '}')
}

// appendMember appends a member's name, after a comma unless it is the first
// of the object that starts at start.
func appendMember(dst []byte, start int, name string) []byte {
	if len(dst) > start {
		dst = append(dst, ',')
	}

	dst = append(dst, '"')
	dst = append(dst, name...)

	return append(dst, '"', ':')
}

// appendScalars appends the S, N, B, NULL and BOOL members of v that are set.
func appendScalars(dst []byte, start int, v *api.AttributeValue) []byte {
	if v.S != nil {
		dst = awsjson.AppendQuote(appendMember(dst, start, "S"), *v.S)
	}

	if v.N != nil {
		dst = awsjson.AppendQuote(appendMember(dst, start, "N"), *v.N)
	}

	if v.B != nil {
		dst = appendBytes(appendMember(dst, start, "B"), v.B)
	}

	if v.NULL != nil {
		dst = strconv.AppendBool(appendMember(dst, start, "NULL"), *v.NULL)
	}

	if v.BOOL != nil {
		dst = strconv.AppendBool(appendMember(dst, start, "BOOL"), *v.BOOL)
	}

	return dst
}

// appendCollections appends the SS, NS, BS, M and L members of v that are set.
func appendCollections(dst []byte, start int, v *api.AttributeValue) []byte {
	if v.SS != nil {
		dst = appendStrings(appendMember(dst, start, "SS"), v.SS)
	}

	if v.NS != nil {
		dst = appendStrings(appendMember(dst, start, "NS"), v.NS)
	}

	if v.BS != nil {
		dst = append(appendMember(dst, start, "BS"), '[')

		for i, b := range v.BS {
			if i > 0 {
				dst = append(dst, ',')
			}

			dst = appendBytes(dst, b)
		}

		dst = append(dst, ']')
	}

	if v.M != nil {
		dst = appendItem(appendMember(dst, start, "M"), v.M)
	}

	if v.L != nil {
		dst = append(appendMember(dst, start, "L"), '[')

		for i := range v.L {
			if i > 0 {
				dst = append(dst, ',')
			}

			dst = appendValue(dst, &v.L[i])
		}

		dst = append(dst, ']')
	}

	return dst
}

func appendStrings(dst []byte, list []string) []byte {
	dst = append(dst, '[')

	for i, s := range list {
		if i > 0 {
			dst = append(dst, ',')
		}

		dst = awsjson.AppendQuote(dst, s)
	}

	return append(dst, ']')
}

func appendBytes(dst []byte, b []byte) []byte {
	dst = append(dst, '"')
	dst = base64.StdEncoding.AppendEncode(dst, b)

	return append(dst, '"')
}

// orderItems are items whose maps the encoder writes in map order: names that
// sort apart once decoded and raw, nested maps, and maps inside lists.
var orderItems = []string{
	`{"a\"":{"S":"1"},"a#":{"S":"2"},"a":{"S":"3"},"a\\":{"S":"4"},"\u0001":{"N":"5"},"é":{"BOOL":true}}`,
	`{"z":{"M":{"y":{"M":{"x":{"S":"1"},"w":{"L":[{"M":{"v":{"N":"1"},"u":{"N":"2"}}},{"S":"t"}]}}},` +
		`"s":{"L":[]},"r":{"M":{}}}},"q":{"L":[{"L":[{"M":{"p":{"NULL":true},"o":{"SS":["n","m"]}}}]}]}}`,
	`{"k9":{"S":"1"},"k10":{"S":"2"},"k1":{"S":"3"},"K":{"BS":["AQ==","Ag=="]},"k":{"NS":["2","1"]}}`,
}

// Test_orderer_writesAppendItem checks the snapshot writes a packed item as
// it wrote the item before items were packed: Orderer gives appendItem's
// bytes for every item, whatever order the encoder put its names in.
func Test_orderer_writesAppendItem(t *testing.T) {
	var o value.Orderer

	for _, text := range slices.Concat(codecItems, orderItems) {
		item, err := newItemParser().parse(jsontext.Value(text))
		require.NoError(t, err, text)
		require.NoError(t, value.ValidateItem(item), text)

		want := appendItem(nil, item)

		// Map order differs between packs of the same item.
		for range 20 {
			p, err := value.Pack(item)
			require.NoError(t, err)
			require.Equal(t, string(want), string(o.Append(nil, p)), text)
		}
	}

	require.Empty(t, o.Append(nil, ""))
}

// FuzzOrderer checks Orderer against appendItem for every item the snapshot
// parser takes and DynamoDB accepts.
func FuzzOrderer(f *testing.F) {
	for _, text := range slices.Concat(codecItems, orderItems) {
		f.Add([]byte(text))
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		if !jsontext.Value(raw).IsValid() {
			return
		}

		item, err := newItemParser().parse(raw)
		if err != nil || value.ValidateItem(item) != nil {
			return
		}

		p, err := value.Pack(item)
		if err != nil {
			return
		}

		var o value.Orderer

		want := appendItem(nil, item)
		require.Equal(t, string(want), string(o.Append(nil, p)))
	})
}

// packHeader is the header of p: its size, scalar count and attribute count.
func packHeader(p value.Packed) string {
	return string(p[:len(p)-len(p.JSON())])
}

// nested is an item whose attribute is depth maps deep, counting itself.
func nested(depth int) string {
	return strings.Repeat(`{"a":{"M":`, depth-1) + `{"a":{"S":"x"}}` + strings.Repeat(`}}`, depth-1)
}

// checkPackedJSON checks what JSONPacker made of raw against the typed path:
// the item parse reads, ValidateItem accepts and Pack packs, with the same
// header, the same item, and raw as the snapshot writes it.
func checkPackedJSON(t *testing.T, raw []byte, got value.Packed) {
	t.Helper()

	item, err := newItemParser().parse(raw)
	require.NoError(t, err, "packed what parse refuses: %s", raw)
	require.NoError(t, value.ValidateItem(item), "packed what ValidateItem refuses: %s", raw)

	want, err := value.Pack(item)
	require.NoError(t, err)
	require.Equal(t, packHeader(want), packHeader(got), string(raw))
	require.True(t, value.EqualItems(item, value.Unpack(got)), string(raw))

	var o value.Orderer

	require.Equal(t, string(appendItem(nil, item)), string(o.Append(nil, got)), string(raw))
}

// Test_jsonPacker_packsSnapshotItems checks JSONPacker takes every item as
// the snapshot writes it, and packs it as Pack packs the item: the same
// header, the same item, and the json written back as it was read.
func Test_jsonPacker_packsSnapshotItems(t *testing.T) {
	var k value.JSONPacker

	texts := slices.Concat(codecItems, orderItems, []string{
		nested(value.MaxDepth), `{}`, `{"n":{"N":"0"},"d":{"N":"-0.001"},"b":{"B":""}}`,
	})

	for _, text := range texts {
		item, err := newItemParser().parse(jsontext.Value(text))
		require.NoError(t, err, text)
		require.NoError(t, value.ValidateItem(item), text)

		raw := appendItem(nil, item)

		got, ok := k.Pack(raw)
		require.True(t, ok, "refused %s", raw)
		checkPackedJSON(t, raw, got)
		require.Equal(t, string(raw), got.JSON(), "the snapshot's json is the packed json")
	}

	_, ok := k.Pack(nil)
	require.False(t, ok)
}

// packerRefuses are items JSONPacker leaves to the typed path: valid json that
// Pack would not write as it is, and items DynamoDB refuses.
var packerRefuses = []string{
	`{ "a":{"S":"x"}}`, `{"a" :{"S":"x"}}`, `{"a":{"S":"x"} }`, `{"a":{ "S":"x"}}`,
	`{"\u0061":{"S":"x"}}`, `{"a":{"S":"\u0078"}}`, `{"a":{"S":"\/"}}`, `{"a":{"\u0053":"x"}}`,
	`{"":{"S":"x"}}`, `{"a":{"S":null}}`, `{"a":{}}`, `{"a":{"S":"x","N":"1"}}`, `{"a":{"X":"x"}}`,
	`{"a":{"N":"01"}}`, `{"a":{"N":"1E2"}}`, `{"a":{"N":"+1"}}`, `{"a":{"N":"-0"}}`, `{"a":{"N":"1.0"}}`,
	`{"a":{"N":"x"}}`, `{"a":{"N":1}}`, `{"a":{"B":"AR=="}}`, `{"a":{"B":"AQ"}}`, `{"a":{"B":"!!"}}`,
	`{"a":{"SS":[]}}`, `{"a":{"SS":["x","x"]}}`, `{"a":{"NS":["1","1"]}}`, `{"a":{"BS":["AQ==","AQ=="]}}`,
	`{"a":{"NULL":false}}`, `{"a":{"BOOL":"true"}}`, `{"a":{"L":{}}}`, `{"a":{"M":[]}}`,
	nested(value.MaxDepth + 1), "{\"a\":{\"S\":\"\xff\"}}", "{\"\xff\":{\"S\":\"x\"}}",
}

// Test_jsonPacker_leavesTheRest checks JSONPacker refuses json it would have
// to rewrite, and items DynamoDB refuses, which the typed path then decodes
// or refuses as it did before.
func Test_jsonPacker_leavesTheRest(t *testing.T) {
	var k value.JSONPacker

	for _, text := range packerRefuses {
		_, ok := k.Pack([]byte(text))
		require.False(t, ok, text)
	}
}

// FuzzJSONPacker checks JSONPacker never takes json the typed path would
// refuse, and that what it takes it packs as the typed path does.
func FuzzJSONPacker(f *testing.F) {
	for _, text := range slices.Concat(codecItems, orderItems, packerRefuses) {
		f.Add([]byte(text))
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		if !jsontext.Value(raw).IsValid(jsontext.AllowInvalidUTF8(true)) {
			return
		}

		var k value.JSONPacker

		got, ok := k.Pack(raw)
		if ok {
			checkPackedJSON(t, raw, got)
		}
	})
}
