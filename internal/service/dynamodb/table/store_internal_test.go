package table

import (
	"maps"
	"slices"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

// requireOrderMatchesParts holds s to its invariant: once its order is
// current, the order lists exactly the keys of parts, sorted.
func requireOrderMatchesParts(t *testing.T, s *store, msg string) {
	t.Helper()

	if s.needsSort() {
		return
	}

	require.Equal(t, slices.Sorted(maps.Keys(s.parts)), s.order, msg)
}

// Test_store_orderFollowsAnEmptiedPartition empties a partition of the table
// and of its index after a Scan built their orders, and checks that neither
// order keeps the dropped key, before or after the next rebuild.
func Test_store_orderFollowsAnEmptiedPartition(t *testing.T) {
	str := func(v string) api.AttributeValue { return api.AttributeValue{S: &v} }

	tbl := New(KeySchema{Hash: KeyAttribute{Name: "pk", Type: "S"}}, []IndexSpec{{
		Name: "byG", Global: true,
		Key: KeySchema{Hash: KeyAttribute{Name: "g", Type: "S"}},
	}})

	for _, pk := range []string{"a", "b", "c"} {
		item := value.Item{"pk": str(pk), "g": str("g" + pk)}
		_, err := tbl.Write(item, func(value.Item) (value.Item, error) { return item, nil })
		require.NoError(t, err)
	}

	indexes := []string{"", "byG"}
	for _, index := range indexes {
		require.NoError(t, tbl.Scan(index, nil, func(value.Item) bool { return true }))
	}

	idx := tbl.index("byG")
	require.False(t, tbl.primary.needsSort())
	require.False(t, idx.data.needsSort())

	_, err := tbl.Write(value.Item{"pk": str("b")}, func(value.Item) (value.Item, error) {
		return nil, nil //nolint:nilnil // a nil item is the engine's delete
	})
	require.NoError(t, err)

	for _, index := range indexes {
		s, _, err := tbl.source(index)
		require.NoError(t, err)

		requireOrderMatchesParts(t, s, "after the delete, index "+index)
		require.Len(t, s.sortedKeys(), 2, index)
		requireOrderMatchesParts(t, s, "after the rebuild, index "+index)
	}
}
