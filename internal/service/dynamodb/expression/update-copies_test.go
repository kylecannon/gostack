package expression_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

// Test_Apply_SiblingsShareOneCopy: the actions under one map or list change one
// copy of it. The row has actions in two maps and two list elements, a REMOVE
// in a list that others SET in, and a SET that appends.
func Test_Apply_SiblingsShareOneCopy(t *testing.T) {
	t.Parallel()

	const (
		oldItem = `{"pk":{"S":"k"},"m":{"M":{"a":{"M":{"x":{"N":"1"}}},"b":{"L":[{"N":"1"},{"N":"2"},{"N":"3"}]}}},` +
			`"l":{"L":[{"M":{"p":{"N":"1"}}},{"M":{"p":{"N":"2"}}}]}}`
		wantItem = `{"pk":{"S":"k"},"m":{"M":{"a":{"M":{"x":{"N":"1"},"y":{"N":"7"},"z":{"N":"7"}}},` +
			`"b":{"L":[{"N":"7"},{"N":"2"}]},"c":{"N":"7"}}},` +
			`"l":{"L":[{"M":{"p":{"N":"1"},"q":{"N":"7"}}},{"M":{"p":{"N":"2"},"q":{"N":"7"}}},{"N":"7"}]}}`
	)

	u, err := expression.ParseUpdate(
		"SET m.a.y = :n, m.a.z = :n, m.b[0] = :n, m.c = :n, l[0].q = :n, l[1].q = :n, l[2] = :n REMOVE m.b[2]",
		updateSubs(t, "", `{":n":{"N":"7"}}`), nil)
	require.NoError(t, err)

	old := updateItem(t, oldItem)
	key := updateItem(t, `{"pk":{"S":"k"}}`)

	got, err := applyUpdate(t, u, key, old)
	require.NoError(t, err)
	require.JSONEq(t, wantItem, updateJSON(t, got))

	// Applied again to the same item stored, which a retry does, it answers the same.
	again, err := applyUpdate(t, u, key, old)
	require.NoError(t, err)
	require.JSONEq(t, wantItem, updateJSON(t, again))
}

// Test_Apply_NestedSetsCopyTheirParentOnce: 1,000 SET actions under one map
// copied that map and walked it for nesting once for each of them, 188 ms an
// UPDATE statement; they copy it once and walk it once (SQ-452). The test counts
// work, the bytes one write's unpack and update allocate, and not time: 788 KB
// fixed, where the copy for each action alone is 103 MB and the walk for each
// action alone 161 MB, and the limit sits between at 4 MiB. Either defect
// restored fails it; a regression under about 5 times the fixed cost does not.
// The bytes a benchmark reports are the process's, so the test runs alone: Go
// runs the tests that call t.Parallel after every other test of the package has
// finished, and none of them is running while this one measures (SQ-456).
func Test_Apply_NestedSetsCopyTheirParentOnce(t *testing.T) { //nolint:paralleltest // measures process allocations
	const actions = 1000

	sets := make([]string, actions)
	for i := range sets {
		sets[i] = "m.u" + strconv.Itoa(i) + " = :n"
	}

	u, err := expression.ParseStatementUpdate("SET "+strings.Join(sets, ", "),
		updateSubs(t, "", `{":n":{"N":"1"}}`), nil, nil)
	require.NoError(t, err)

	stored := packItem(t, updateItem(t, `{"pk":{"S":"k"},"m":{"M":{"k0":{"N":"1"}}}}`))
	key := updateItem(t, `{"pk":{"S":"k"}}`)

	got, err := applyUpdate(t, u, key, updateItem(t, `{"pk":{"S":"k"},"m":{"M":{"k0":{"N":"1"}}}}`))
	require.NoError(t, err)
	require.Len(t, got["m"].M, actions+1)

	res := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			_, _ = expression.ApplyStored(key, stored, func(value.Item) (*expression.UpdateExpr, error) {
				return u, nil
			})
		}
	})

	t.Logf("bytes of one ApplyStored of %d nested SET actions: %d", actions, res.AllocedBytesPerOp())
	require.Less(t, res.AllocedBytesPerOp(), int64(4<<20), "bytes of one ApplyStored of %d nested SET actions", actions)
}

// Test_ApplyStored_CopiesBelowTheItem: ApplyStored writes the update into the
// item it unpacked and returns it, but a map below an attribute is still copied
// before an action changes it. In SET a = b, b.x = :v the new a is the old b,
// which shares b's map: written in place, b.x = :v would change a too, and the
// map read saw under b.
func Test_ApplyStored_CopiesBelowTheItem(t *testing.T) {
	t.Parallel()

	u, err := expression.ParseUpdate("SET a = b, b.x = :n", updateSubs(t, "", `{":n":{"N":"7"}}`), nil)
	require.NoError(t, err)

	key := updateItem(t, `{"pk":{"S":"k"}}`)
	stored := packItem(t, updateItem(t, `{"pk":{"S":"k"},"b":{"M":{"x":{"N":"1"}}}}`))

	var (
		unpacked value.Item
		oldB     value.Item
	)

	got, err := expression.ApplyStored(key, stored, func(old value.Item) (*expression.UpdateExpr, error) {
		unpacked, oldB = old, old["b"].M

		return u, nil
	})
	require.NoError(t, err)
	require.JSONEq(t, `{"pk":{"S":"k"},"a":{"M":{"x":{"N":"1"}}},"b":{"M":{"x":{"N":"7"}}}}`, updateJSON(t, got))
	require.Equal(t, "1", *oldB["x"].N, "the map under the old b is not written")
	require.Contains(t, unpacked, "a", "the item read saw is the item written")

	// With nothing stored, ApplyStored makes the item.
	got, err = applyUpdate(t, u, key, nil)
	requireUpdateError(t, err, "The provided expression refers to an attribute that does not exist in the item")
	require.Nil(t, got)

	set, err := expression.ParseUpdate("SET c = :n", updateSubs(t, "", `{":n":{"N":"7"}}`), nil)
	require.NoError(t, err)

	got, err = applyUpdate(t, set, key, nil)
	require.NoError(t, err)
	require.JSONEq(t, `{"pk":{"S":"k"},"c":{"N":"7"}}`, updateJSON(t, got))
}

// Test_ApplyStored_ReadDecides: read's error stops the write before the update
// runs, and with no update the item is answered as stored, or the key alone
// when nothing is.
func Test_ApplyStored_ReadDecides(t *testing.T) {
	t.Parallel()

	key := updateItem(t, `{"pk":{"S":"k"}}`)
	stored := packItem(t, updateItem(t, `{"pk":{"S":"k"},"a":{"S":"x"}}`))
	refused := value.Invalid("refused")

	got, err := expression.ApplyStored(key, stored, func(value.Item) (*expression.UpdateExpr, error) {
		return nil, refused
	})
	require.ErrorIs(t, err, refused)
	require.Nil(t, got)

	none := func(value.Item) (*expression.UpdateExpr, error) { return nil, nil } //nolint:nilnil // nil is no update

	got, err = expression.ApplyStored(key, stored, none)
	require.NoError(t, err)
	require.JSONEq(t, `{"pk":{"S":"k"},"a":{"S":"x"}}`, updateJSON(t, got))

	got, err = expression.ApplyStored(key, "", none)
	require.NoError(t, err)
	require.JSONEq(t, `{"pk":{"S":"k"}}`, updateJSON(t, got))
}
