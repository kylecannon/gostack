package dynamodb

import (
	"strconv"
	"strings"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/partiql"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

// selectPaths are the paths of `SELECT <list> FROM t`.
func selectPaths(tb testing.TB, list string) []partiql.Path {
	tb.Helper()

	stmt, err := partiql.Parse("SELECT " + list + " FROM t")
	require.NoError(tb, err)

	return stmt.Projection
}

// padded is list followed by n paths no item below holds, which takes it past
// shapeHintMax, where the result is sized from what the item holds.
func padded(list string, n int) string {
	missing := make([]string, n)
	for i := range missing {
		missing[i] = "pad.z" + strconv.Itoa(i)
	}

	return list + ", " + strings.Join(missing, ", ")
}

// Test_shapeSelected_sameAsShapeItem: the result of a SELECT is the same item
// whether shapeItem counts the paths the whole item holds or shapeSelected
// sizes it from the parts the decoder kept for them, for a path at the top
// level, in a map, below a list index and cut at one, missing, repeated, named
// by an alias another path takes, and overlapped by a path that takes its
// parent whole. A list of more than shapeHintMax paths is sized from what was
// kept, so each row runs short and padded with paths nothing holds.
func Test_shapeSelected_sameAsShapeItem(t *testing.T) {
	str := func(s string) api.AttributeValue { return api.AttributeValue{S: &s} }
	mapOf := func(m value.Item) api.AttributeValue { return api.AttributeValue{M: m} }

	inner := value.Item{"deep": str("z")}
	members := value.Item{"k0": str("x"), "k1": str("y"), "n": mapOf(inner)}
	list := []api.AttributeValue{
		mapOf(value.Item{"a": str("a0")}),
		mapOf(value.Item{"a": str("a1"), "b": str("b1")}),
		str("l2"),
	}

	item := value.Item{
		"pk": str("a"),
		"k0": str("top"),
		"m":  mapOf(members),
		"l":  {L: list},
		"x":  mapOf(value.Item{"a": str("xa")}),
	}

	packed, err := value.Pack(item)
	require.NoError(t, err)

	var dec value.Decoder
	defer dec.Release()

	for _, tt := range []struct {
		name string
		sel  string
		want value.Item
	}{
		{"top level", "k0, nope.a", value.Item{"k0": str("top")}},
		{"map member", "m.k0, m.k1, m.none", value.Item{"k0": str("x"), "k1": str("y")}},
		{"deeper map", "m.n.deep, m.n.none", value.Item{"deep": str("z")}},
		{"index zero", "l[0].a", value.Item{"a": str("a0")}},
		{"cut at an index", "l[1].a, l[1].b", value.Item{"l[1]": mapOf(value.Item{"a": str("a1"), "b": str("b1")})}},
		{"cut at a scalar element", "l[2]", value.Item{"l[2]": str("l2")}},
		{"index past the list", "l[9].a, l[0].a", value.Item{"a": str("a0")}},
		{"missing", "nope.a, m.nope", value.Item{}},
		{"repeated", "m.k0, m.k0", value.Item{"k0": str("x")}},
		{"one alias", "m.k0, x.a, m.k1", value.Item{"a": str("xa"), "k0": str("x"), "k1": str("y")}},
		{"parent whole", "m, m.k0", value.Item{"m": mapOf(members), "k0": str("x")}},
	} {
		for _, long := range []bool{false, true} {
			list := tt.sel
			if long {
				list = padded(list, 2*shapeHintMax)
			}

			t.Run(tt.name+"/"+strconv.FormatBool(long), func(t *testing.T) {
				paths := selectPaths(t, list)

				before := shapeItem(item, paths)
				after := shapeSelected(dec.ViewSelect(packed, selectionOf(paths)), paths)

				require.Equal(t, tt.want, before)
				require.Equal(t, before, after)
			})
		}
	}
}

// Test_shapeSelected_allocations: the result of a long list is sized from what
// the decoder kept, as shapeItem sizes it from what the item holds: 2 held of
// 1,000 paths costs less than 8 KiB an item (SQ-412), and 64 held of 64 costs
// less than the 15 KiB a map that grew from small does not (SQ-456).
func Test_shapeSelected_allocations(t *testing.T) {
	holder := func(n int) value.Packed {
		members := make(value.Item, n)

		for i := range n {
			s := "x" + strconv.Itoa(i)
			members["k"+strconv.Itoa(i)] = api.AttributeValue{S: &s}
		}

		packed, err := value.Pack(value.Item{"m": {M: members}})
		require.NoError(t, err)

		return packed
	}

	for _, tt := range []struct {
		name        string
		held, paths int
		maxBytes    int64
	}{
		{"sparse", 2, 1000, 8 << 10},
		{"full", 64, 64, 15 << 10},
	} {
		names := make([]string, tt.paths)
		for i := range names {
			names[i] = "m.k" + strconv.Itoa(i)
		}

		paths := selectPaths(t, strings.Join(names, ", "))
		sel := selectionOf(paths)
		packed := holder(tt.held)

		var dec value.Decoder

		view := dec.ViewSelect(packed, sel)

		require.Len(t, shapeSelected(view, paths), tt.held)

		res := testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()

			for b.Loop() {
				_ = shapeSelected(view, paths)
			}
		})

		dec.Release()

		require.Less(t, res.AllocedBytesPerOp(), tt.maxBytes, "bytes of the result of %d held of %d paths", tt.held, tt.paths)
	}
}
