package expression_test

import (
	"encoding/json"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

// updateFuzzItem is the old item the fuzz targets and the property test start
// from, and updateFuzzValues the substitutions they parse with.
const (
	updateFuzzItem = `{"pk":{"S":"k"},"a":{"N":"1"},"b":{"M":{"x":{"S":"1"},"y":{"N":"2"}}},` +
		`"c":{"L":[{"S":"0"},{"S":"1"},{"S":"2"}]},"d":{"SS":["a","b"]},"e":{"NS":["1","2"]}}`
	updateFuzzValues = `{":n":{"N":"2.5"},":s":{"S":"x"},":l":{"L":[{"S":"y"}]},":ss":{"SS":["b","c"]},` +
		`":ns":{"NS":["2","3"]},":m":{"M":{"z":{"BOOL":true}}}}`
	updateFuzzNames = `{"#a":"a","#b":"b","#k":"pk"}`
)

// updateWalk reads path from item, written apart from the package so the
// property below does not check the package against itself.
func updateWalk(item value.Item, path expression.Path) (api.AttributeValue, bool) {
	cur := api.AttributeValue{M: item}

	for _, el := range path {
		switch {
		case el.Name != "" && cur.M != nil:
			next, ok := cur.M[el.Name]
			if !ok {
				return api.AttributeValue{}, false
			}

			cur = next
		case el.Name == "" && el.Index < len(cur.L):
			cur = cur.L[el.Index]
		default:
			return api.AttributeValue{}, false
		}
	}

	return cur, true
}

// requireUpdatedNew checks the UPDATED_NEW property: every path the update
// named that is there in the new item reads the same from the Attributes, and
// every attribute returned is the start of such a path.
func requireUpdatedNew(t *testing.T, u *expression.UpdateExpr, old, updated value.Item) {
	t.Helper()

	got := expression.ReturnValues(expression.ReturnUpdatedNew, u.Paths(), old, updated)
	starts := map[string]bool{}

	for _, path := range u.Paths() {
		want, ok := updateWalk(updated, path)
		if !ok {
			continue
		}

		starts[path[0].Name] = true

		have, ok := updateWalk(got, path)
		require.True(t, ok, "UPDATED_NEW lacks %v", path)
		require.True(t, value.Equal(&want, &have), "UPDATED_NEW differs at %v", path)
	}

	for name := range got {
		require.True(t, starts[name], "UPDATED_NEW returns %q, which no written path reaches", name)
	}
}

// updateFuzzApply parses expr over the fuzz substitutions and applies it to
// the fuzz item as a write does. It checks the key survives and the
// UPDATED_NEW property holds.
func updateFuzzApply(t *testing.T, expr string) {
	t.Helper()

	u, err := expression.ParseUpdate(expr, updateSubs(t, updateFuzzNames, updateFuzzValues), nil)
	if err != nil {
		return
	}

	old := updateItem(t, updateFuzzItem)

	updated, err := applyUpdate(t, u, updateItem(t, updateKey), old)
	if err != nil {
		return
	}

	require.Equal(t, "k", *updated["pk"].S)
	requireUpdatedNew(t, u, old, updated)

	for name, v := range updated {
		require.NoError(t, value.Validate(&v), "attribute %q", name)
	}
}

var updateFuzzSeeds = []string{
	"SET a = :n", "SET b.x = :s, c[1] = :s", "SET c[9] = :s REMOVE c[0]", "REMOVE b.y, c[2], a",
	"ADD a :n, e :ns DELETE d :ss", "SET a = if_not_exists(z, :n) + a", "SET c = list_append(c, :l)",
	"SET #a = (:n - a), #b.w = :m", "SET q.r = :s", "SET   a   =    =  :v", "SET a[01] = :n", "set a = :n remove b",
	"SET c[0][0] = :s", "DELETE c[1] :ss", "ADD b.y :n", "REMOVE #k",
}

// FuzzParseUpdate holds the parser to never panicking on any text, and to
// paths that start at an attribute name.
func FuzzParseUpdate(f *testing.F) {
	for _, seed := range updateFuzzSeeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, expr string) {
		u, err := expression.ParseUpdate(expr, updateSubs(t, updateFuzzNames, updateFuzzValues), nil)
		if err != nil {
			return
		}

		for _, path := range u.Paths() {
			require.NotEmpty(t, path[0].Name)
		}
	})
}

// FuzzUpdateApply holds the applier to its contract on any expression that
// parses: no panic, old untouched, valid values, and the UPDATED_NEW property.
func FuzzUpdateApply(f *testing.F) {
	for _, seed := range updateFuzzSeeds {
		f.Add(seed)
	}

	f.Fuzz(updateFuzzApply)
}

// Test_UpdateExpr_UpdatedNew checks the UPDATED_NEW property over updates
// drawn from a pool of actions of every clause.
func Test_UpdateExpr_UpdatedNew(t *testing.T) {
	t.Parallel()

	pool := map[string][]string{
		"SET": {
			"a = :n", "b.x = :s", "c[1] = :s", "c[7] = :s", "e = list_append(:l, :l)",
			"f = if_not_exists(a, :n) + :n",
		},
		"REMOVE": {"b.y", "c[0]", "c[2]", "d", "g"},
		"ADD":    {"h :n", "e :ns", "b.z :n"},
		"DELETE": {"d :ss", "e :ns"},
	}
	rng := rand.New(rand.NewPCG(1, 2)) // a fixed seed, for reproducible draws

	for range 500 {
		var clauses []string

		for clause, actions := range pool {
			var picked []string

			for _, act := range actions {
				if rng.IntN(3) == 0 {
					picked = append(picked, act)
				}
			}

			if len(picked) > 0 {
				clauses = append(clauses, clause+" "+strings.Join(picked, ", "))
			}
		}

		if len(clauses) > 0 {
			updateFuzzApply(t, strings.Join(clauses, " "))
		}
	}
}

// BenchmarkUpdateApply measures Apply by the number of SET actions and the
// size of the item they update: the update alone, on an item already decoded,
// as main measured it. BenchmarkUpdateApplyStored adds the unpack a write does.
func BenchmarkUpdateApply(b *testing.B) {
	benchUpdateApplyMatrix(b, func(b *testing.B, u *expression.UpdateExpr, key, old value.Item) {
		b.Helper()

		var err error

		for b.Loop() {
			_, err = u.Apply(key, old)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkUpdateApplyStored measures ApplyStored as a write runs it: it
// unpacks the item stored and applies the update into it, so each op reads the
// whole item, which Apply's copy of the top-level map does not. The end to end
// cost is BenchmarkUpdateItem's payload rows.
func BenchmarkUpdateApplyStored(b *testing.B) {
	benchUpdateApplyMatrix(b, func(b *testing.B, u *expression.UpdateExpr, key, old value.Item) {
		b.Helper()

		stored := packItem(b, old)
		read := func(value.Item) (*expression.UpdateExpr, error) { return u, nil }

		for b.Loop() {
			_, err := expression.ApplyStored(key, stored, read)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

// benchUpdateApplyMatrix runs apply on each cell: an update of clauses SET
// actions on an item of a key and a string of the cell's size.
func benchUpdateApplyMatrix(b *testing.B, apply func(b *testing.B, u *expression.UpdateExpr, key, old value.Item)) {
	b.Helper()

	for _, clauses := range []int{1, 4, 16} {
		for _, size := range []struct {
			name  string
			bytes int
		}{{"1KiB", 1 << 10}, {"64KiB", 64 << 10}, {"390KiB", 390 << 10}} {
			b.Run("clauses="+strconv.Itoa(clauses)+"/item="+size.name, func(b *testing.B) {
				u := benchUpdateExpr(b, clauses)
				key := value.Item{"pk": {S: new("k")}}
				old := value.Item{"pk": {S: new("k")}, "pad": {S: new(strings.Repeat("x", size.bytes))}}

				b.SetBytes(int64(size.bytes))
				apply(b, u, key, old)
			})
		}
	}
}

// benchUpdateExpr is an update of clauses SET actions, each of a function and
// an addition.
func benchUpdateExpr(b *testing.B, clauses int) *expression.UpdateExpr {
	b.Helper()

	actions := make([]string, clauses)
	for i := range actions {
		actions[i] = "a" + strconv.Itoa(i) + " = if_not_exists(a" + strconv.Itoa(i) + ", :z) + :n"
	}

	values, err := json.Marshal(map[string]api.AttributeValue{":z": {N: new("0")}, ":n": {N: new("1")}})
	require.NoError(b, err)

	var subs expression.Substitutions
	require.NoError(b, json.Unmarshal(values, &subs.Values))

	u, err := expression.ParseUpdate("SET "+strings.Join(actions, ", "), subs, nil)
	require.NoError(b, err)

	return u
}
