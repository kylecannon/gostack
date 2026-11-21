package dynamodb_test

import (
	"strconv"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/partiql"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

// shapePaths are the paths of `SELECT <prefix>0, ..., <prefix>(n-1) FROM t`.
func shapePaths(tb testing.TB, prefix string, n int) []partiql.Path {
	tb.Helper()

	stmt, err := partiql.Parse("SELECT " + selectList(prefix, n) + " FROM t")
	require.NoError(tb, err)

	return stmt.Projection
}

// shapeHolder is an item whose map m holds k0 to k(n-1).
func shapeHolder(n int) value.Item {
	members := make(map[string]api.AttributeValue, n)

	for i := range n {
		x := "x" + strconv.Itoa(i)
		members["k"+strconv.Itoa(i)] = api.AttributeValue{S: &x}
	}

	id := "a"

	return value.Item{"id": {S: &id}, "m": {M: members}}
}

// Test_shapeItem_allocations: a narrow nested SELECT builds each result in a map
// sized for its paths, and a list of a thousand that an item holds two of does
// not size it for the list. The counts are exact for the pinned toolchain. Each
// path costs the allocation of its alias; of the rest, the result of 10 paths
// held takes 14 with the size and 15 without it, and of 30 paths 34 and 39
// (SQ-452). Sized for the list's length, the result of a thousand paths costs
// every item more than 100 KiB, and unsized costs it little (SQ-412).
func Test_shapeItem_allocations(t *testing.T) {
	for _, tt := range []struct {
		paths    int
		wantRest float64
	}{{10, 14}, {30, 34}} {
		item := shapeHolder(tt.paths)
		paths := shapePaths(t, "m.k", tt.paths)

		require.Len(t, dynamodb.ShapeResult(item, paths), tt.paths)

		aliases := testing.AllocsPerRun(100, func() {
			for _, p := range paths {
				_ = p.Alias()
			}
		})
		shaped := testing.AllocsPerRun(100, func() { _ = dynamodb.ShapeResult(item, paths) })

		require.InDelta(t, tt.wantRest, shaped-aliases, 0, "allocations besides the aliases of %d held paths", tt.paths)
	}

	wide := shapePaths(t, "m.k", 1000)
	holder := shapeHolder(2)

	require.Len(t, dynamodb.ShapeResult(holder, wide), 2)

	res := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			_ = dynamodb.ShapeResult(holder, wide)
		}
	})

	require.Less(t, res.AllocedBytesPerOp(), int64(8<<10), "bytes of the result of 2 held of 1,000 paths")
}
