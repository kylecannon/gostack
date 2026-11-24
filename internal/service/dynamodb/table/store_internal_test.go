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
		_, err := tbl.Write(item, func(value.Packed) (value.Item, error) { return item, nil })
		require.NoError(t, err)
	}

	require.NoError(t, tbl.Scan("", nil, func(string, value.Packed) bool { return true }))

	_, err := tbl.Write(value.Item{"pk": str("b")}, func(value.Packed) (value.Item, error) {
		return nil, nil //nolint:nilnil // a nil item is the engine's delete
	})
	require.NoError(t, err)

	require.False(t, tbl.primary.needsSort(), "a delete leaves the order current")
	require.Len(t, tbl.primary.order, 3, "the order keeps the emptied partition's key")
	require.Len(t, tbl.primary.parts, 2)

	pk := func(packed value.Packed) string { return *value.Unpack(packed)["pk"].S }

	tbl.mu.RLock()
	img := tbl.ImageHeld()
	tbl.mu.RUnlock()

	var imaged []string
	require.NoError(t, img.Each(func(packed value.Packed) error {
		imaged = append(imaged, pk(packed))
		return nil
	}))
	require.Equal(t, []string{"a", "c"}, imaged, "ImageHeld")

	var scanned []string
	require.NoError(t, tbl.Scan("", nil, func(_ string, packed value.Packed) bool {
		scanned = append(scanned, pk(packed))
		return true
	}))
	require.Equal(t, []string{"a", "c"}, scanned, "Scan")

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
