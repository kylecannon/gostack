package expression_test

import (
	"maps"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

// spliceNames are the names the generator gives attributes and map members:
// plain ones, and ones the encoder escapes or writes as more than one byte.
var spliceNames = []string{"a", "b", "c", "é", `q"t`, "n\nl", `\s`, "\x01", "long" + strings.Repeat("n", 40)}

// spliceStrings are the strings the generator writes, the same mix.
var spliceStrings = []string{"", "x", "é", `q"`, `\`, "\n", " ", strings.Repeat("y", 300)}

// spliceNumbers are numbers as a request writes them; validation puts each
// in canonical form.
var spliceNumbers = []string{
	"0", "1", "-2.5", "1e3", "0.50", "12345678901234567890123456789012345678", "-1E-130", "9.9e125",
}

// The placeholders of the sets an update adds and deletes, and the clauses.
const (
	spliceSS     = ":ss"
	spliceNS     = ":ns"
	clauseSet    = "SET"
	clauseRemove = "REMOVE"
	clauseAdd    = "ADD"
	clauseDelete = "DELETE"
)

// spliceGen draws items and updates from one seeded source.
type spliceGen struct {
	rng *rand.Rand
}

func (g *spliceGen) pick(list []string) string {
	return list[g.rng.IntN(len(list))]
}

// value draws an attribute value; depth bounds its nesting.
func (g *spliceGen) value(depth int) api.AttributeValue {
	const kinds = 10

	k := g.rng.IntN(kinds)
	if depth == 0 && k >= 8 {
		k = 0
	}

	switch k {
	case 0:
		return api.AttributeValue{S: new(g.pick(spliceStrings))}
	case 1:
		return api.AttributeValue{N: new(g.pick(spliceNumbers))}
	case 2:
		b := make([]byte, g.rng.IntN(5))
		for i := range b {
			b[i] = byte(g.rng.IntN(256))
		}

		return api.AttributeValue{B: b}
	case 3:
		return api.AttributeValue{BOOL: new(g.rng.IntN(2) == 0)}
	case 4:
		return api.AttributeValue{NULL: new(true)}
	case 5:
		return api.AttributeValue{SS: g.distinct([]string{"x", "é", `q"`, "\n"})}
	case 6:
		return api.AttributeValue{NS: g.distinct([]string{"1", "2", "-3", "4.5", "1e7"})}
	case 7:
		return api.AttributeValue{BS: [][]byte{{1}, {2, 3}}[:1+g.rng.IntN(2)]}
	case 8:
		m := value.Item{}
		for range g.rng.IntN(4) {
			m[g.pick(spliceNames)] = g.value(depth - 1)
		}

		return api.AttributeValue{M: m}
	default:
		l := []api.AttributeValue{}
		for range g.rng.IntN(4) {
			l = append(l, g.value(depth-1))
		}

		return api.AttributeValue{L: l}
	}
}

// distinct draws one to three different members of list.
func (g *spliceGen) distinct(list []string) []string {
	out := make([]string, 0, 3)
	for _, i := range g.rng.Perm(len(list))[:1+g.rng.IntN(3)] {
		out = append(out, list[i])
	}

	return out
}

// item draws an item keyed by pk. Now and then it holds enough attributes, or
// a string long enough, that the header's counts and size take more than one
// byte each.
func (g *spliceGen) item() value.Item {
	item := value.Item{"pk": {S: new("k")}}

	for range g.rng.IntN(6) {
		item[g.pick(spliceNames)] = g.value(3)
	}

	if g.rng.IntN(8) == 0 {
		for i := range 130 {
			item["f"+strconv.Itoa(i)] = api.AttributeValue{N: new(strconv.Itoa(i))}
		}
	}

	if g.rng.IntN(8) == 0 {
		item["big"] = api.AttributeValue{S: new(strings.Repeat("z", 20<<10))}
	}

	return item
}

// spliceValues are the values an update draws from.
func spliceValues() map[string]api.AttributeValue {
	return map[string]api.AttributeValue{
		":s":     {S: new("v")},
		":n":     {N: new("2")},
		spliceSS: {SS: []string{"x", "é"}},
		spliceNS: {NS: []string{"1", "2"}},
		":l":     {L: []api.AttributeValue{{S: new("e")}}},
		":m":     {M: value.Item{`q"t`: {N: new("1")}}},
	}
}

// update draws an update expression of one to four actions, each on its own
// attribute: one the item holds, one it does not, and now and then the key,
// which the update refuses. Now and then a second action writes below the
// first one's attribute.
func (g *spliceGen) update(item value.Item) (string, map[string]string) {
	targets := []string{"new", "big"}
	for _, name := range slices.Sorted(maps.Keys(item)) {
		if name != "big" && (name != "pk" || g.rng.IntN(8) == 0) {
			targets = append(targets, name)
		}
	}

	names := map[string]string{"#m": g.pick(spliceNames), "#o": "other"}
	clauses := map[string][]string{}
	order := g.rng.Perm(len(targets))
	actions := min(1+g.rng.IntN(4), len(targets))

	for i := range actions {
		x, y := "#x"+strconv.Itoa(i), "#y"+strconv.Itoa(i)
		names[x], names[y] = targets[order[i]], g.pick(targets)
		v := g.pick([]string{":s", ":n", spliceSS, spliceNS, ":l", ":m"})

		clause, action := g.action(x, y, v)
		clauses[clause] = append(clauses[clause], action)
	}

	// Two actions below one map the item holds, which no other action writes.
	for _, j := range order[actions:] {
		if name := targets[j]; item[name].M != nil && g.rng.IntN(2) == 0 {
			names["#z"] = name
			clauses[clauseSet] = append(clauses[clauseSet], "#z.#m = :s", "#z.#o = :n")

			break
		}
	}

	var expr []string
	for _, clause := range []string{clauseSet, clauseRemove, clauseAdd, clauseDelete} {
		if len(clauses[clause]) > 0 {
			expr = append(expr, clause+" "+strings.Join(clauses[clause], ", "))
		}
	}

	return strings.Join(expr, " "), names
}

// action draws one action on x, which may read y and the value v.
func (g *spliceGen) action(x, y, v string) (string, string) {
	actions := [][2]string{
		{clauseSet, x + " = " + v},
		{clauseSet, x + " = " + v},
		{clauseSet, x + " = " + v},
		{clauseSet, x + " = " + y},
		{clauseRemove, x},
		{clauseSet, x + ".#m = " + v},
		{clauseSet, x + "[0] = " + v},
		{clauseSet, x + "[9] = " + v},
		{clauseSet, x + " = list_append(" + x + ", :l)"},
		{clauseSet, x + " = if_not_exists(" + y + ", " + v + ")"},
		{clauseSet, x + " = " + y + " + :n"},
		{clauseRemove, x},
		{clauseRemove, x + ".#m"},
		{clauseRemove, x + "[0]"},
		{clauseAdd, x + " " + g.pick([]string{":n", spliceSS, spliceNS})},
		{clauseDelete, x + " " + g.pick([]string{spliceSS, spliceNS})},
	}
	a := actions[g.rng.IntN(len(actions))]

	return a[0], a[1]
}

// requireSpliceMatches applies an update drawn from seed to an item drawn
// from it, stored as Pack and as JSONPacker store it, and holds
// ApplyStoredSpliced to ApplyStored: the same item or the same error, and the
// item packed as Pack packs it, the same header and, with each map's names
// in order, the same json.
func requireSpliceMatches(t *testing.T, seed1, seed2 uint64) int {
	t.Helper()

	g := spliceGen{rng: rand.New(rand.NewPCG(seed1, seed2))}
	item := g.item()

	if value.ValidateItem(item) != nil {
		return 0
	}

	text, names := g.update(item)

	u, err := expression.ParseUpdate(text, expression.Substitutions{Names: names, Values: spliceValues()}, nil)
	if err != nil {
		return 0
	}

	packed, err := value.Pack(item)
	require.NoError(t, err)

	var (
		o  value.Orderer
		jp value.JSONPacker
	)

	fromJSON, ok := jp.Pack(o.Append(nil, packed))
	require.True(t, ok, "JSONPacker refuses the json Orderer writes")

	key := value.Item{"pk": item["pk"]}
	spliced := 0

	for r, read := range []func(value.Item) (*expression.UpdateExpr, error){
		func(value.Item) (*expression.UpdateExpr, error) { return u, nil },
		func(value.Item) (*expression.UpdateExpr, error) {
			return nil, nil //nolint:nilnil // read returns no update and no error
		},
	} {
		for _, stored := range []value.Packed{packed, fromJSON} {
			want, wantErr := expression.ApplyStored(key, stored, read)
			got, gotPacked, gotErr := expression.ApplyStoredSpliced(key, stored, read)

			if wantErr != nil {
				require.EqualError(t, gotErr, wantErr.Error(), "update %q", text)

				continue
			}

			require.NoError(t, gotErr, "update %q", text)
			require.True(t, value.EqualItems(want, got), "update %q", text)
			require.NotEmpty(t, gotPacked, "update %q", text)

			wantPacked, err := value.Pack(want)
			require.NoError(t, err)

			requireSamePacked(t, wantPacked, gotPacked, text)

			if r == 0 {
				spliced++
			}
		}
	}

	return spliced
}

// requireSamePacked holds got to want, packed from the same item: the same
// header, and the same json once each map's names are in order.
func requireSamePacked(t *testing.T, want, got value.Packed, what string) {
	t.Helper()

	require.Equal(t, packedHeader(want), packedHeader(got), "header (size, scalars, attributes) of %q", what)

	var o value.Orderer

	require.Equal(t, string(o.Append(nil, want)), string(o.Append(nil, got)), "json of %q", what)
}

// packedHeader is the bytes of p before its json: the item's size, the count
// of its S and N values and the count of its attributes.
func packedHeader(p value.Packed) string {
	return string(p)[:len(p)-len(p.JSON())]
}

// Test_Splice_MatchesPack is the differential property over many draws: an
// update spliced into the stored item packs it as unpacking, applying and
// packing does. Most draws make an update that fails or does not parse, so
// the count of updates spliced is held too, lest the property hold of none.
func Test_Splice_MatchesPack(t *testing.T) {
	t.Parallel()

	const draws = 6000

	spliced := 0
	for i := range uint64(draws) {
		spliced += requireSpliceMatches(t, i, 0x5eed)
	}

	t.Logf("%d of %d updates spliced", spliced, 2*draws)
	require.Greater(t, spliced, draws/4, "updates spliced")
}

// FuzzSplice is the property of Test_Splice_MatchesPack over any seed.
func FuzzSplice(f *testing.F) {
	for i := range uint64(8) {
		f.Add(i, uint64(1))
	}

	f.Fuzz(func(t *testing.T, seed1, seed2 uint64) { requireSpliceMatches(t, seed1, seed2) })
}

// Test_ApplyStoredPacked_writesHalf: an update that writes less than half the
// stored item is spliced into it, and one that writes half or more is left
// for the caller to pack, as is an update of no stored item.
func Test_ApplyStoredPacked_writesHalf(t *testing.T) {
	t.Parallel()

	item := value.Item{"pk": {S: new("k")}, "n": {N: new("1")}, "d": {S: new(strings.Repeat("x", 1<<10))}}
	stored, err := value.Pack(item)
	require.NoError(t, err)

	key := value.Item{"pk": item["pk"]}
	values := map[string]api.AttributeValue{":n": {N: new("2")}, ":d": {S: new(strings.Repeat("y", 1<<10))}}

	tests := []struct {
		name        string
		giveUpdate  string
		wantSpliced bool
	}{
		{name: "a number beside a large string", giveUpdate: "SET n = :n", wantSpliced: true},
		{name: "the large string", giveUpdate: "SET d = :d", wantSpliced: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			u, err := expression.ParseUpdate(tt.giveUpdate, expression.Substitutions{Values: values}, nil)
			require.NoError(t, err)

			read := func(value.Item) (*expression.UpdateExpr, error) { return u, nil }

			got, packed, err := expression.ApplyStoredPacked(key, stored, read)
			require.NoError(t, err)
			require.Equal(t, tt.wantSpliced, packed != "")

			if tt.wantSpliced {
				want, packErr := value.Pack(got)
				require.NoError(t, packErr)
				requireSamePacked(t, want, packed, tt.name)
			}

			_, packed, err = expression.ApplyStoredPacked(key, "", read)
			require.NoError(t, err)
			require.Empty(t, packed, "no stored item")
		})
	}
}
