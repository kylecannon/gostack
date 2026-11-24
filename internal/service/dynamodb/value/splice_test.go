package value_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

// The items the cases of Test_Splice start from.
const (
	spliceA   = `{"a":{"N":"1"}}`
	spliceABC = `{"a":{"N":"1"},"b":{"S":"x"},"c":{"BOOL":true}}`
)

// requireSamePacked holds got to want, packed from the same item: the same
// header, and the same json once each map's names are in order.
func requireSamePacked(t *testing.T, want, got value.Packed, what string) {
	t.Helper()

	require.Equal(t, packedHeader(want), packedHeader(got), "header (size, scalars, attributes) of %q", what)

	var o value.Orderer

	require.Equal(t, string(o.Append(nil, want)), string(o.Append(nil, got)), "json of %q", what)
}

// packedHeader is the bytes of p before its json: the item's size, the count
// of its S and N values and the count of its attributes.
func packedHeader(p value.Packed) string {
	return string(p)[:len(p)-len(p.JSON())]
}

// Test_Splice covers the shapes of a splice one by one: which members it
// copies, where the commas fall, and an item left with no attribute.
func Test_Splice(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		giveItem   string
		giveNames  []string
		giveValues string
		wantItem   string
	}{
		{
			name:       "replace the first member",
			giveItem:   spliceABC,
			giveNames:  []string{"a"},
			giveValues: `{"a":{"N":"2"}}`,
			wantItem:   `{"a":{"N":"2"},"b":{"S":"x"},"c":{"BOOL":true}}`,
		},
		{
			name:       "remove the last member",
			giveItem:   spliceABC,
			giveNames:  []string{"c"},
			giveValues: `{}`,
			wantItem:   `{"a":{"N":"1"},"b":{"S":"x"}}`,
		},
		{
			name:       "remove a middle member and add one",
			giveItem:   spliceABC,
			giveNames:  []string{"b", "d"},
			giveValues: `{"d":{"SS":["p","q"]}}`,
			wantItem:   `{"a":{"N":"1"},"c":{"BOOL":true},"d":{"SS":["p","q"]}}`,
		},
		{
			name:       "remove every member",
			giveItem:   spliceA,
			giveNames:  []string{"a"},
			giveValues: `{}`,
			wantItem:   `{}`,
		},
		{
			name:       "replace every member",
			giveItem:   `{"a":{"N":"1"},"b":{"S":"x"}}`,
			giveNames:  []string{"a", "b"},
			giveValues: `{"a":{"S":"y"},"b":{"M":{"z":{"NULL":true}}}}`,
			wantItem:   `{"a":{"S":"y"},"b":{"M":{"z":{"NULL":true}}}}`,
		},
		{
			name:       "a name the encoder escapes",
			giveItem:   `{"q\"t":{"N":"1"},"a\n":{"S":"x"}}`,
			giveNames:  []string{"a\n"},
			giveValues: `{"a\n":{"S":"y "}}`,
			wantItem:   `{"q\"t":{"N":"1"},"a\n":{"S":"y "}}`,
		},
		{
			name:       "a name the item does not hold, removed",
			giveItem:   spliceA,
			giveNames:  []string{"z"},
			giveValues: `{}`,
			wantItem:   spliceA,
		},
		{
			name:       "a large member replaced by a small one",
			giveItem:   `{"a":{"S":"` + strings.Repeat("x", 64<<10) + `"},"b":{"N":"1"}}`,
			giveNames:  []string{"a"},
			giveValues: `{"a":{"S":"y"}}`,
			wantItem:   `{"a":{"S":"y"},"b":{"N":"1"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stored, err := value.Pack(spliceItem(t, tt.giveItem))
			require.NoError(t, err)

			got, err := value.Splice(stored, tt.giveNames, spliceItem(t, tt.giveValues))
			require.NoError(t, err)

			want, err := value.Pack(spliceItem(t, tt.wantItem))
			require.NoError(t, err)

			requireSamePacked(t, want, got, tt.name)
		})
	}
}

// Test_Splice_Unchanged: a splice that reaches no attribute returns the
// stored item itself, and one of no stored item returns none.
func Test_Splice_Unchanged(t *testing.T) {
	t.Parallel()

	stored, err := value.Pack(spliceItem(t, spliceA))
	require.NoError(t, err)

	got, err := value.Splice(stored, []string{"z"}, value.Item{})
	require.NoError(t, err)
	require.Equal(t, stored, got)

	got, err = value.Splice(stored, nil, nil)
	require.NoError(t, err)
	require.Equal(t, stored, got, "no names")

	got, err = value.Splice("", []string{"a"}, value.Item{"a": {N: new("2")}})
	require.NoError(t, err)
	require.Empty(t, got)
}

// spliceItem decodes and validates an item's json.
func spliceItem(t *testing.T, s string) value.Item {
	t.Helper()

	var item value.Item
	require.NoError(t, json.Unmarshal([]byte(s), &item), s)
	require.NoError(t, value.ValidateItem(item), "%.80s", s)

	return item
}
