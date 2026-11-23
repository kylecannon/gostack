package table_test

import (
	"errors"
	"strconv"
	"sync"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/awserr"
	"github.com/kylecannon/gostack/internal/service/dynamodb/table"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	keyA1      = "a/1"
	keyPMinus1 = "p/-1"
	keyA2      = "a/2"
	keyP10     = "p/10"
	keyP100    = "p/100"
	keyP2dot5  = "p/2.5"
	keyP9      = "p/9"
)

func s(v string) api.AttributeValue { return api.AttributeValue{S: &v} }
func n(v string) api.AttributeValue { return api.AttributeValue{N: &v} }

// composite is a table keyed by pk (S) and sk (N), with a global index on g
// (S) and a local one on pk and l (S).
func composite() *table.Table {
	key := table.KeySchema{
		Hash:  table.KeyAttribute{Name: "pk", Type: "S"},
		Range: table.KeyAttribute{Name: "sk", Type: "N"},
	}

	return table.New(key, []table.IndexSpec{
		{
			Name:       "byG",
			Global:     true,
			Key:        table.KeySchema{Hash: table.KeyAttribute{Name: "g", Type: "S"}},
			Projection: table.Projection{Type: table.ProjectKeysOnly},
		},
		{
			Name:       "byL",
			Key:        table.KeySchema{Hash: key.Hash, Range: table.KeyAttribute{Name: "l", Type: "S"}},
			Projection: table.Projection{Type: table.ProjectAll},
		},
	})
}

func put(t *testing.T, tbl *table.Table, item value.Item) table.Change {
	t.Helper()

	require.NoError(t, value.ValidateItem(item))

	change, err := tbl.Write(item, func(value.Item) (value.Item, error) { return item, nil })
	require.NoError(t, err)

	return change
}

func remove(t *testing.T, tbl *table.Table, key value.Item) table.Change {
	t.Helper()

	change, err := tbl.Write(key, func(value.Item) (value.Item, error) {
		return nil, nil //nolint:nilnil // a nil item is the engine's delete
	})
	require.NoError(t, err)

	return change
}

func keysOf(
	t *testing.T, tbl *table.Table, index string, hash api.AttributeValue,
	cond table.SortCondition, start value.Item, forward bool,
) []string {
	t.Helper()

	var got []string

	require.NoError(t, tbl.Range(index, &hash, cond, start, forward, func(packed value.Packed) bool {
		item := value.Unpack(packed)

		got = append(got, *item["pk"].S+"/"+*item["sk"].N)

		return true
	}))

	return got
}

func Test_Table_indexesFollowOverwriteAndDelete(t *testing.T) {
	tbl := composite()

	put(t, tbl, value.Item{"pk": s("a"), "sk": n("1"), "g": s("x"), "l": s("m")})
	put(t, tbl, value.Item{"pk": s("a"), "sk": n("2"), "g": s("x")})
	put(t, tbl, value.Item{"pk": s("b"), "sk": n("1")})

	assert.Equal(t, []string{keyA1, keyA2}, keysOf(t, tbl, "byG", s("x"), table.SortCondition{}, nil, true))
	assert.Equal(t, []string{keyA1}, keysOf(t, tbl, "byL", s("a"), table.SortCondition{}, nil, true),
		"sparse: a/2 has no l")

	// Overwrite a/1 with another g and no l: it leaves both old index entries.
	change := put(t, tbl, value.Item{"pk": s("a"), "sk": n("1.0"), "g": s("y")})
	assert.Equal(t, "x", *change.Old["g"].S)
	assert.Equal(t, []string{keyA2}, keysOf(t, tbl, "byG", s("x"), table.SortCondition{}, nil, true))
	assert.Equal(t, []string{keyA1}, keysOf(t, tbl, "byG", s("y"), table.SortCondition{}, nil, true))
	assert.Empty(t, keysOf(t, tbl, "byL", s("a"), table.SortCondition{}, nil, true))

	change = remove(t, tbl, value.Item{"pk": s("a"), "sk": n("2")})
	assert.NotNil(t, change.Old)
	assert.Nil(t, change.New)
	assert.Empty(t, keysOf(t, tbl, "byG", s("x"), table.SortCondition{}, nil, true))

	count, _ := tbl.Stats()
	assert.Equal(t, 2, count)

	gCount, _, ok := tbl.IndexStats("byG")
	require.True(t, ok)
	assert.Equal(t, 1, gCount)
}

func Test_Table_Range(t *testing.T) {
	tbl := composite()
	for _, sk := range []string{"10", "9", "-1", "2.5", "100"} {
		put(t, tbl, value.Item{"pk": s("p"), "sk": n(sk)})
	}

	put(t, tbl, value.Item{"pk": s("q"), "sk": n("5")})

	tests := []struct {
		name        string
		giveCond    table.SortCondition
		giveStart   value.Item
		giveForward bool
		want        []string
	}{
		{name: "all forward", giveForward: true, want: []string{keyPMinus1, keyP2dot5, keyP9, keyP10, keyP100}},
		{name: "all backward", want: []string{keyP100, keyP10, keyP9, keyP2dot5, keyPMinus1}},
		{
			name:        "greater than",
			giveCond:    table.SortCondition{Op: table.SortGT, Values: []api.AttributeValue{n("9")}},
			giveForward: true,
			want:        []string{keyP10, keyP100},
		},
		{
			name:        "at most",
			giveCond:    table.SortCondition{Op: table.SortLE, Values: []api.AttributeValue{n("9")}},
			giveForward: true,
			want:        []string{keyPMinus1, keyP2dot5, keyP9},
		},
		{
			name:        "between",
			giveCond:    table.SortCondition{Op: table.SortBetween, Values: []api.AttributeValue{n("2.5"), n("10")}},
			giveForward: true, want: []string{keyP2dot5, keyP9, keyP10},
		},
		{
			name:        "equal",
			giveCond:    table.SortCondition{Op: table.SortEQ, Values: []api.AttributeValue{n("10")}},
			giveForward: true,
			want:        []string{keyP10},
		},
		{
			name:        "after a start key",
			giveStart:   value.Item{"pk": s("p"), "sk": n("2.5")},
			giveForward: true,
			want:        []string{keyP9, keyP10, keyP100},
		},
		{
			name:      "before a start key backward",
			giveStart: value.Item{"pk": s("p"), "sk": n("10")},
			want:      []string{keyP9, keyP2dot5, keyPMinus1},
		},
		{
			name:        "start key that is not stored",
			giveStart:   value.Item{"pk": s("p"), "sk": n("3")},
			giveForward: true,
			want:        []string{keyP9, keyP10, keyP100},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, keysOf(t, tbl, "", s("p"), tt.giveCond, tt.giveStart, tt.giveForward))
		})
	}
}

func Test_Table_Range_beginsWith(t *testing.T) {
	key := table.KeySchema{
		Hash:  table.KeyAttribute{Name: "pk", Type: "S"},
		Range: table.KeyAttribute{Name: "sk", Type: "S"},
	}
	tbl := table.New(key, nil)

	for _, sk := range []string{"ab", "abc", "b", "a", "abd"} {
		put(t, tbl, value.Item{"pk": s("p"), "sk": s(sk)})
	}

	var got []string

	prefix := table.SortCondition{Op: table.SortBeginsWith, Values: []api.AttributeValue{s("ab")}}
	require.NoError(t, tbl.Range("", new(s("p")), prefix, nil, true,
		func(packed value.Packed) bool {
			item := value.Unpack(packed)

			got = append(got, *item["sk"].S)

			return true
		}))
	assert.Equal(t, []string{"ab", "abc", "abd"}, got)
}

func Test_Table_Range_indexStartKey(t *testing.T) {
	tbl := composite()
	for i := range 4 {
		put(t, tbl, value.Item{"pk": s("p"), "sk": n(strconv.Itoa(i)), "g": s("same")})
	}

	start := value.Item{"pk": s("p"), "sk": n("1"), "g": s("same")}
	assert.Equal(t, []string{"p/2", "p/3"}, keysOf(t, tbl, "byG", s("same"), table.SortCondition{}, start, true),
		"items sharing an index key are ordered by table key")

	err := tbl.Range("nope", new(s("p")), table.SortCondition{}, nil, true, func(value.Packed) bool { return true })
	require.ErrorIs(t, err, table.ErrNoIndex)
}

func Test_Table_Scan(t *testing.T) {
	tbl := composite()
	for _, pk := range []string{"c", "a", "b"} {
		for _, sk := range []string{"2", "1"} {
			put(t, tbl, value.Item{"pk": s(pk), "sk": n(sk)})
		}
	}

	scan := func(start value.Item, limit int) []string {
		var got []string

		require.NoError(t, tbl.Scan("", start, func(_ string, packed value.Packed) bool {
			item := value.Unpack(packed)

			got = append(got, *item["pk"].S+"/"+*item["sk"].N)

			return len(got) < limit
		}))

		return got
	}

	first := scan(nil, 3)
	assert.Equal(t, []string{keyA1, keyA2, "b/1"}, first)
	assert.Equal(t, []string{"b/2", "c/1", "c/2"}, scan(value.Item{"pk": s("b"), "sk": n("1")}, 10))

	remove(t, tbl, value.Item{"pk": s("b"), "sk": n("1")})
	remove(t, tbl, value.Item{"pk": s("b"), "sk": n("2")})
	assert.Equal(t, []string{"c/1", "c/2"}, scan(value.Item{"pk": s("b"), "sk": n("1")}, 10),
		"a start in a partition that is gone")
}

func Test_Table_AddIndexBackfillsAndDropIndex(t *testing.T) {
	key := table.KeySchema{Hash: table.KeyAttribute{Name: "pk", Type: "S"}}
	tbl := table.New(key, nil)

	put(t, tbl, value.Item{"pk": s("a"), "q": s("x")})
	put(t, tbl, value.Item{"pk": s("b")})

	tbl.AddIndex(table.IndexSpec{
		Name: "byQ", Global: true,
		Key: table.KeySchema{Hash: table.KeyAttribute{Name: "q", Type: "S"}},
	})

	count, _, ok := tbl.IndexStats("byQ")
	require.True(t, ok)
	assert.Equal(t, 1, count)

	assert.True(t, tbl.DropIndex("byQ"))
	assert.False(t, tbl.DropIndex("byQ"))
}

func Test_Table_Write_decideError(t *testing.T) {
	tbl := composite()
	put(t, tbl, value.Item{"pk": s("a"), "sk": n("1")})

	refused := errors.New("refused")
	change, err := tbl.Write(value.Item{"pk": s("a"), "sk": n("1")}, func(value.Item) (value.Item, error) {
		return nil, refused
	})

	require.ErrorIs(t, err, refused)
	assert.NotNil(t, change.Old)
	assert.NotNil(t, tbl.Get(value.Item{"pk": s("a"), "sk": n("1")}), "a refused write stores nothing")
}

func Test_Change_Changed(t *testing.T) {
	item := value.Item{"pk": s("a")}
	same := value.Item{"pk": s("a")}
	other := value.Item{"pk": s("a"), "x": n("1")}

	assert.False(t, table.Change{Old: item, New: same}.Changed())
	assert.True(t, table.Change{Old: item, New: other}.Changed())
	assert.True(t, table.Change{New: item}.Changed())
	assert.True(t, table.Change{Old: item}.Changed())
	assert.False(t, table.Change{}.Changed())
}

func Test_Table_CheckItem(t *testing.T) {
	tbl := composite()
	long := make([]byte, 2049)

	for i := range long {
		long[i] = 'x'
	}

	tests := []struct {
		name string
		give value.Item
		want string
	}{
		{
			name: "missing range key",
			give: value.Item{"pk": s("a")},
			want: "One of the required keys was not given a value",
		},
		{
			name: "key of another type",
			give: value.Item{"pk": n("1"), "sk": n("1")},
			want: "One or more parameter values were invalid: Type mismatch for key",
		},
		{
			name: "empty key string", give: value.Item{"pk": s(""), "sk": n("1")},
			want: "One or more parameter values are not valid. " +
				"The AttributeValue for a key attribute cannot contain an empty string value. Key: pk",
		},
		{
			name: "hash key too long", give: value.Item{"pk": s(string(long)), "sk": n("1")},
			want: "Hash primary key values must be under 2048 bytes, and range primary key values must be under 1024 bytes",
		},
		{
			name: "index key of another type",
			give: value.Item{"pk": s("a"), "sk": n("1"), "g": n("1")},
			want: "One or more parameter values were invalid: Type mismatch for Index Key",
		},
		{
			name: "empty index key", give: value.Item{"pk": s("a"), "sk": n("1"), "g": s("")},
			want: "One or more parameter values are not valid. A value specified for a secondary index key is not supported. " +
				"The AttributeValue for a key attribute cannot contain an empty string value. IndexName: byG, IndexKey: g",
		},
		{
			name: "too large",
			give: value.Item{"pk": s("a"), "sk": n("1"), "d": s(string(make([]byte, value.MaxItemSize)))},
			want: "Item size has exceeded the maximum allowed size",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tbl.CheckItem(tt.give)

			awsErr, ok := errors.AsType[*awserr.Error](err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, tt.want, awsErr.Message)
		})
	}
}

func Test_Table_CheckKey(t *testing.T) {
	tbl := composite()

	tests := []struct {
		name string
		give value.Item
		want string
	}{
		{name: "too few", give: value.Item{"pk": s("a")}, want: "The number of conditions on the keys is invalid"},
		{
			name: "too many",
			give: value.Item{"pk": s("a"), "sk": n("1"), "x": s("q")},
			want: "The number of conditions on the keys is invalid",
		},
		{
			name: "wrong name",
			give: value.Item{"PK": s("a"), "sk": n("1")},
			want: "One of the required keys was not given a value",
		},
		{name: "set", give: value.Item{"pk": {SS: []string{"a"}}, "sk": n("1")}, want: "Invalid attribute value type"},
		{
			name: "wrong type",
			give: value.Item{"pk": s("a"), "sk": s("1")},
			want: "One or more parameter values were invalid: Type mismatch for key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			awsErr, ok := errors.AsType[*awserr.Error](tbl.CheckKey(tt.give))
			require.True(t, ok)
			assert.Equal(t, tt.want, awsErr.Message)
		})
	}
}

func Test_Lock_overlappingTables(t *testing.T) {
	a, b := composite(), composite()

	var wg sync.WaitGroup

	for i := range 50 {
		wg.Go(func() {
			tables := []*table.Table{a, b, a}
			if i%2 == 1 {
				tables = []*table.Table{b, a}
			}

			unlock := table.Lock(tables...)
			defer unlock()

			for _, tbl := range []*table.Table{a, b} {
				item := value.Item{"pk": s("k"), "sk": n(strconv.Itoa(i))}
				_, err := tbl.WriteHeld(item, func(value.Item) (value.Item, error) { return item, nil })
				assert.NoError(t, err)
			}
		})
	}

	wg.Wait()

	count, _ := a.Stats()
	assert.Equal(t, 50, count)
}

func Test_IndexSpec_Project(t *testing.T) {
	key := table.KeySchema{Hash: table.KeyAttribute{Name: "pk", Type: "S"}}
	spec := table.IndexSpec{
		Name: "i", Key: table.KeySchema{Hash: table.KeyAttribute{Name: "g", Type: "S"}},
		Projection: table.Projection{Type: table.ProjectInclude, NonKeyAttributes: []string{"x"}},
	}
	item := value.Item{"pk": s("a"), "g": s("b"), "x": s("c"), "y": s("d")}

	assert.Equal(t, value.Item{"pk": s("a"), "g": s("b"), "x": s("c")}, spec.Project(key, item))

	spec.Projection = table.Projection{Type: table.ProjectKeysOnly}
	assert.Equal(t, value.Item{"pk": s("a"), "g": s("b")}, spec.Project(key, item))
}
