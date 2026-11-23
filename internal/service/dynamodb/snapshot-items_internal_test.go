package dynamodb

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

// codecItems are items of every attribute type, with the strings the codec
// must escape and the empty values it must keep apart from absent ones.
var codecItems = []string{
	`{"s":{"S":"plain"},"n":{"N":"-1.5E+3"},"b":{"B":"AAEC/w=="},"t":{"BOOL":true},"f":{"BOOL":false},` +
		`"z":{"NULL":true},"ss":{"SS":["a","b"]},"ns":{"NS":["1","2.50"]},"bs":{"BS":["AQ==","Ag=="]},` +
		`"m":{"M":{"y":{"S":"1"},"x":{"L":[{"N":"0"},{"M":{}},{"L":[]}]}}},"l":{"L":[]},"e":{"M":{}}}`,
	`{"esc\"ape\\d\u0001":{"S":"quote\" slash\\ tab\t nl\n é 😀 </script>"},"eb":{"B":""},"es":{"S":""}}`,
	`{"id":{"S":"escaped names"},"b":{"B":"AQ=="},"end":{"S":"ends\\"},"q":{"S":"\"\\\""}}`,
}

// Test_itemCodec_roundTrip parses each item, compares it with what json v2
// reads, and writes it back: writing what parse read gives the same item, and
// the same bytes each time.
func Test_itemCodec_roundTrip(t *testing.T) {
	p := newItemParser()

	for _, text := range codecItems {
		got, err := p.parse(jsontext.Value(text))
		require.NoError(t, err, text)
		require.NoError(t, value.ValidateItem(got))

		var want value.Item

		require.NoError(t, json.Unmarshal([]byte(text), &want))
		require.NoError(t, value.ValidateItem(want))
		require.Equal(t, want, got, text)

		out := appendItem(nil, got)
		require.True(t, jsontext.Value(out).IsValid(), string(out))

		again, err := p.parse(out)
		require.NoError(t, err)
		require.Equal(t, got, again)

		twice := appendItem(nil, again)
		require.Equal(t, string(out), string(twice))
	}
}

// FuzzItemParser parses two items with one parser, so names shared from the
// first reach the second. An item parse takes must be what json v2 reads from
// the same bytes, and must come back unchanged through appendItem.
func FuzzItemParser(f *testing.F) {
	for _, text := range codecItems {
		f.Add([]byte(text), []byte(`{"a":{"S":"b"}}`))
	}

	for _, pair := range collidingNames {
		f.Add([]byte(pair[0]), []byte(pair[1]))
		f.Add([]byte(pair[1]), []byte(pair[0]))
	}

	f.Fuzz(func(t *testing.T, first, second []byte) {
		p := newItemParser()

		for _, raw := range [][]byte{first, second} {
			if !jsontext.Value(raw).IsValid() {
				continue
			}

			got, err := p.parse(raw)
			if err != nil {
				continue
			}

			var want value.Item

			require.NoError(t, json.Unmarshal(raw, &want), "parse took what json v2 refuses")
			require.Equal(t, want, got)

			out := appendItem(nil, got)
			again, err := newItemParser().parse(out)
			require.NoError(t, err)
			require.Equal(t, got, again)
		}
	})
}

// Test_itemCodec_emptyValues checks an empty binary, list, map and set member
// come back present, not absent: absent would leave no member set.
func Test_itemCodec_emptyValues(t *testing.T) {
	got, err := newItemParser().parse(jsontext.Value(`{"b":{"B":""},"l":{"L":[]},"m":{"M":{}},"s":{"S":""}}`))
	require.NoError(t, err)

	for name, v := range got {
		require.Equal(t, 1, v.MembersSet(), name)
	}
}

// Test_itemCodec_sharedNames checks items share their attribute names and own
// their values.
func Test_itemCodec_sharedNames(t *testing.T) {
	p := newItemParser()
	raw := []byte(`{"name":{"S":"v"}}`)

	a, err := p.parse(raw)
	require.NoError(t, err)

	copy(raw, `{"name":{"S":"w"}}`)

	b, err := p.parse(raw)
	require.NoError(t, err)
	require.Equal(t, "v", *a["name"].S, "a value must not alias the raw bytes")
	require.Equal(t, "w", *b["name"].S)
	require.Len(t, p.names, 1)
}

// Test_itemCodec_escapedNames checks names whose raw forms are another's
// decoded form stay apart across the items of one parser, in either order.
func Test_itemCodec_escapedNames(t *testing.T) {
	for _, pair := range collidingNames {
		for _, items := range [][2]string{pair, {pair[1], pair[0]}} {
			p := newItemParser()

			for _, text := range items {
				got, err := p.parse(jsontext.Value(text))
				require.NoError(t, err, text)

				var want value.Item

				require.NoError(t, json.Unmarshal([]byte(text), &want))
				require.Equal(t, want, got, text)
			}
		}
	}
}

// collidingNames are pairs of items whose attribute name's raw json form is
// the other's decoded name: a\"b and a"b, "x" and x.
var collidingNames = [][2]string{
	{`{"a\\\"b":{"S":"1"}}`, `{"a\"b":{"S":"2"}}`},
	{`{"\"x\"":{"S":"1"}}`, `{"x":{"S":"2"}}`},
}

// Test_itemCodec_deepest checks an item nested as deep as value.Validate
// allows is written and read back.
func Test_itemCodec_deepest(t *testing.T) {
	levels := value.MaxDepth - 1
	text := `{"a":` + strings.Repeat(`{"L":[`, levels) + `{"S":"x"}` + strings.Repeat(`]}`, levels) + `}`

	got, err := newItemParser().parse(jsontext.Value(text))
	require.NoError(t, err)
	require.NoError(t, value.ValidateItem(got))

	out := appendItem(nil, got)
	require.Equal(t, text, string(out))
}

// Test_itemCodec_refuses checks values no snapshot writes are refused, not
// read as something else.
func Test_itemCodec_refuses(t *testing.T) {
	for _, text := range []string{
		`[]`,
		`{"a":{"X":"1"}}`,
		`{"a":{"S":1}}`,
		`{"a":{"S":null}}`,
		`{"a":{"B":"not base64!"}}`,
		`{"a":{"BOOL":"true"}}`,
		`{"a":{"SS":"a"}}`,
		`{"a":{"L":[1]}}`,
		`{"a":"x"}`,
		`{"a":{"S":"x"},"a":{"S":"y"}}`,
	} {
		_, err := newItemParser().parse(jsontext.Value(text))
		require.ErrorIs(t, err, errSnapshot, text)
	}
}
