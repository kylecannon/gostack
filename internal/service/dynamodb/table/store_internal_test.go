package table

import (
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

// Test_store_walkersSkipADeadKey empties a partition after a Scan built the
// order, so the order is current but holds a key with no partition, and runs
// every walker of the order on it: Scan, ImageHeld and AddIndex.
func Test_store_walkersSkipADeadKey(t *testing.T) {
	str := func(v string) api.AttributeValue { return api.AttributeValue{S: &v} }

	tbl := New(KeySchema{Hash: KeyAttribute{Name: "pk", Type: "S"}}, nil)

	for _, pk := range []string{"a", "b", "c"} {
		item := value.Item{"pk": str(pk), "g": str("g" + pk)}
		_, err := tbl.Write(item, func(value.Item) (value.Item, error) { return item, nil })
		require.NoError(t, err)
	}

	require.NoError(t, tbl.Scan("", nil, func(value.Item) bool { return true }))

	_, err := tbl.Write(value.Item{"pk": str("b")}, func(value.Item) (value.Item, error) {
		return nil, nil //nolint:nilnil // a nil item is the engine's delete
	})
	require.NoError(t, err)

	require.False(t, tbl.primary.needsSort(), "a delete leaves the order current")
	require.Len(t, tbl.primary.order, 3, "the order keeps the emptied partition's key")
	require.Len(t, tbl.primary.parts, 2)

	pks := func(items []value.Item) []string {
		got := make([]string, 0, len(items))
		for _, item := range items {
			got = append(got, *item["pk"].S)
		}

		return got
	}

	tbl.mu.RLock()
	img := tbl.ImageHeld()
	tbl.mu.RUnlock()
	require.Equal(t, []string{"a", "c"}, pks(img.Items()), "ImageHeld")

	var scanned []value.Item
	require.NoError(t, tbl.Scan("", nil, func(item value.Item) bool {
		scanned = append(scanned, item)
		return true
	}))
	require.Equal(t, []string{"a", "c"}, pks(scanned), "Scan")

	require.NotPanics(t, func() {
		tbl.AddIndex(IndexSpec{
			Name: "byG", Global: true,
			Key: KeySchema{Hash: KeyAttribute{Name: "g", Type: "S"}},
		})
	}, "AddIndex")

	count, _, ok := tbl.IndexStats("byG")
	require.True(t, ok)
	require.Equal(t, 2, count, "AddIndex backfills the two items left")
}
