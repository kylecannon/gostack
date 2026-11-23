package table_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service/dynamodb/table"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

// scanKeys is every item of index (the table for ""), in Scan order, with its
// g and l.
func scanKeys(t *testing.T, tbl *table.Table, index string) []string {
	t.Helper()

	var got []string

	require.NoError(t, tbl.Scan(index, nil, func(_ string, packed value.Packed) bool {
		item := value.Unpack(packed)

		key := *item["pk"].S + "/" + *item["sk"].N
		if g, ok := item["g"]; ok {
			key += " g=" + *g.S
		}

		if l, ok := item["l"]; ok {
			key += " l=" + *l.S
		}

		got = append(got, key)

		return true
	}))

	return got
}

// loadItems are items over three partitions, some in each index, listed out
// of key order.
func loadItems() []value.Item {
	const count = 30

	items := make([]value.Item, 0, count)

	for i := range count {
		item := value.Item{"pk": s("p" + strconv.Itoa(i%3)), "sk": n(strconv.Itoa(29 - i))}
		if i%2 == 0 {
			item["g"] = s("g" + strconv.Itoa(i%4))
		}

		if i%5 != 0 {
			item["l"] = s("l" + strconv.Itoa(i%7))
		}

		items = append(items, item)
	}

	return items
}

// Test_Table_Load checks a table filled by Load reads like one filled by
// Write: the same items in the same order through the table and each index,
// the same counts and sizes, and writes after the load find their place.
func Test_Table_Load(t *testing.T) {
	written, loaded := composite(), composite()

	for _, item := range loadItems() {
		put(t, written, item)
	}

	require.NoError(t, loaded.Load(loadItems()))

	for _, tbl := range []*table.Table{written, loaded} {
		put(t, tbl, value.Item{"pk": s("p1"), "sk": n("15"), "g": s("g1"), "l": s("l0")})
		put(t, tbl, value.Item{"pk": s("p9"), "sk": n("0"), "g": s("g0")})
		remove(t, tbl, value.Item{"pk": s("p0"), "sk": n("29")})
	}

	for _, index := range []string{"", "byG", "byL"} {
		require.Equal(t, scanKeys(t, written, index), scanKeys(t, loaded, index), index)

		if index == "" {
			continue
		}

		wc, ws, _ := written.IndexStats(index)
		lc, ls, _ := loaded.IndexStats(index)
		require.Equal(t, []int{wc, ws}, []int{lc, ls}, index)
	}

	wc, ws := written.Stats()
	lc, ls := loaded.Stats()
	require.Equal(t, []int{wc, ws}, []int{lc, ls})
}

// Test_Table_ImageHeld checks an image lists the items as Scan reads them,
// whether or not the table's partition order is current, and that writes
// after it leave it as it was.
func Test_Table_ImageHeld(t *testing.T) {
	tbl := composite()
	require.NoError(t, tbl.Load(loadItems()))

	image := func() []string {
		unlock := table.ReadLock(tbl)
		img := tbl.ImageHeld()
		unlock()

		var got []string

		require.NoError(t, img.Each(func(packed value.Packed) error {
			item := value.Unpack(packed)
			got = append(got, *item["pk"].S+"/"+*item["sk"].N)

			return nil
		}))

		return got
	}

	keys := func() []string {
		scanned := scanKeys(t, tbl, "")

		got := make([]string, 0, len(scanned))
		for _, key := range scanned {
			got = append(got, strings.Fields(key)[0])
		}

		return got
	}

	require.Equal(t, keys(), image(), "after Load, order not built")
	require.Equal(t, keys(), image(), "after a Scan built the order")

	unlock := table.ReadLock(tbl)
	img := tbl.ImageHeld()
	unlock()

	put(t, tbl, value.Item{"pk": s("p0"), "sk": n("100")})
	put(t, tbl, value.Item{"pk": s("a"), "sk": n("1")})
	remove(t, tbl, value.Item{"pk": s("p1"), "sk": n("1")})

	held := 0

	require.NoError(t, img.Each(func(value.Packed) error {
		held++

		return nil
	}))
	require.Equal(t, len(loadItems()), held)
	require.Equal(t, keys(), image(), "after writes that add a partition")

	// The order is current (keys() scanned); emptying a partition leaves its
	// key in it.
	remove(t, tbl, value.Item{"pk": s("a"), "sk": n("1")})
	require.Equal(t, keys(), image(), "after a delete that empties a partition")
}

// Test_Table_Load_hashOnly checks a table without a range key, which Load
// fills in one pass.
func Test_Table_Load_hashOnly(t *testing.T) {
	key := table.KeySchema{Hash: table.KeyAttribute{Name: "pk", Type: "S"}}
	tbl := table.New(key, nil)

	require.NoError(t, tbl.Load([]value.Item{{"pk": s("b")}, {"pk": s("a")}, {"pk": s("c")}}))
	put(t, tbl, value.Item{"pk": s("ab")})

	var got []string

	require.NoError(t, tbl.Scan("", nil, func(_ string, packed value.Packed) bool {
		item := value.Unpack(packed)

		got = append(got, *item["pk"].S)

		return true
	}))
	require.Equal(t, []string{"a", "ab", "b", "c"}, got)
}

// Test_Table_Load_refuses checks two items with one key, and a table that
// already holds items, are refused.
func Test_Table_Load_refuses(t *testing.T) {
	key := table.KeySchema{Hash: table.KeyAttribute{Name: "pk", Type: "S"}}
	require.ErrorIs(t, table.New(key, nil).Load([]value.Item{{"pk": s("a")}, {"pk": s("a")}}), table.ErrDuplicateKey)

	twice := []value.Item{{"pk": s("a"), "sk": n("1")}, {"pk": s("a"), "sk": n("1")}}
	require.ErrorIs(t, composite().Load(twice), table.ErrDuplicateKey)

	tbl := composite()
	put(t, tbl, twice[0])
	require.Error(t, tbl.Load(twice[1:]))
}
