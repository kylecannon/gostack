package dynamodb

import (
	"encoding/base64"
	"encoding/json/jsontext"
	"maps"
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
