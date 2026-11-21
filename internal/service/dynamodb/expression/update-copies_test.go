package expression_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/stretchr/testify/require"
)

// Test_Apply_SiblingsShareOneCopy: the actions under one map or list change one
// copy of it, and leave the old item as it was. The row has actions in two maps
// and two list elements, a REMOVE in a list that others SET in, and a SET that
// appends.
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

	got, err := u.Apply(key, old)
	require.NoError(t, err)
	require.JSONEq(t, wantItem, updateJSON(t, got))
	require.JSONEq(t, oldItem, updateJSON(t, old), "the old item is as it was")

	// Applied again to the same old item, which a retry does, it answers the same.
	again, err := u.Apply(key, old)
	require.NoError(t, err)
	require.JSONEq(t, wantItem, updateJSON(t, again))
}

// Test_Apply_NestedSetsCopyTheirParentOnce: 1,000 SET actions under one map
// copied that map for each of them, 188 ms an UPDATE statement and a gigabyte
// of garbage for the 1,000; they copy it once, and the whole update allocates
// a few megabytes (SQ-452).
func Test_Apply_NestedSetsCopyTheirParentOnce(t *testing.T) {
	t.Parallel()

	const actions = 1000

	sets := make([]string, actions)
	for i := range sets {
		sets[i] = "m.u" + strconv.Itoa(i) + " = :n"
	}

	u, err := expression.ParseStatementUpdate("SET "+strings.Join(sets, ", "),
		updateSubs(t, "", `{":n":{"N":"1"}}`), nil, nil)
	require.NoError(t, err)

	old := updateItem(t, `{"pk":{"S":"k"},"m":{"M":{"k0":{"N":"1"}}}}`)
	key := updateItem(t, `{"pk":{"S":"k"}}`)

	got, err := u.Apply(key, old)
	require.NoError(t, err)
	require.Len(t, got["m"].M, actions+1)
	require.Len(t, old["m"].M, 1, "the old item is as it was")

	res := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			_, _ = u.Apply(key, old)
		}
	})

	require.Less(t, res.AllocedBytesPerOp(), int64(4<<20), "bytes of one Apply of %d nested SET actions", actions)
}
