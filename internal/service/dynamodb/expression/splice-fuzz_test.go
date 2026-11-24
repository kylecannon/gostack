package expression_test

import (
	"encoding/json"
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
var spliceStrings = []string{"", "x", "é", `q"`, `\`, "\n", " ", strings.Repeat("y", 300)}

// spliceNumbers are numbers as a request writes them; validation puts each
// in canonical form.
var spliceNumbers = []string{
	"0", "1", "-2.5", "1e3", "0.50", "12345678901234567890123456789012345678", "-1E-130", "9.9e125",
}

// The names of the key attributes, the placeholders of the sets an update
// adds and deletes, and the clauses.
const (
	splicePK     = "pk"
	spliceSK     = "sk"
	spliceSS     = ":ss"
	spliceNS     = ":ns"
	spliceL      = ":l"
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
		return api.AttributeValue{B: g.bytes(5)}
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

// bytes draws up to n random bytes.
func (g *spliceGen) bytes(n int) []byte {
	b := make([]byte, g.rng.IntN(n))
	for i := range b {
		b[i] = byte(g.rng.IntN(256))
	}

	return b
}

// distinct draws one to three different members of list.
func (g *spliceGen) distinct(list []string) []string {
	out := make([]string, 0, 3)
	for _, i := range g.rng.Perm(len(list))[:1+g.rng.IntN(3)] {
		out = append(out, list[i])
	}

	return out
}

// keyValue draws a value a key attribute holds: a string, a number or a
// binary, none of them empty.
func (g *spliceGen) keyValue() api.AttributeValue {
	switch g.rng.IntN(3) {
	case 0:
		return api.AttributeValue{S: new(g.pick(spliceStrings[1:]))}
	case 1:
		return api.AttributeValue{N: new(g.pick(spliceNumbers))}
	default:
		return api.AttributeValue{B: append(g.bytes(4), 7)}
	}
}

// item draws an item keyed by pk, and by sk now and then, and returns it with
// its key. Now and then it holds enough attributes, or a string long enough,
// that the header's counts and size take more than one byte each.
func (g *spliceGen) item() (value.Item, value.Item) {
	key := value.Item{splicePK: g.keyValue()}
	if g.rng.IntN(2) == 0 {
		key[spliceSK] = g.keyValue()
	}

	item := maps.Clone(key)

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

	return item, key
}

// spliceValues are the values an update draws from.
func spliceValues() map[string]api.AttributeValue {
	return map[string]api.AttributeValue{
		":s":     {S: new("v")},
		":n":     {N: new("2")},
		spliceSS: {SS: []string{"x", "é"}},
		spliceNS: {NS: []string{"1", "2"}},
		spliceL:  {L: []api.AttributeValue{{S: new("e")}}},
		":m":     {M: value.Item{`q"t`: {N: new("1")}}},
	}
}

// spliceUpdate is an update drawn for an item: its text, for a failure, and
// read, which returns it for the item stored.
type spliceUpdate struct {
	text string
	read func(old value.Item) (*expression.UpdateExpr, error)
}

// targets are the attributes an update of item draws from: one the item
// holds, one it does not, and now and then a key attribute, which the update
// refuses.
func (g *spliceGen) targets(item value.Item) []string {
	targets := []string{"new", "big"}
	for _, name := range slices.Sorted(maps.Keys(item)) {
		isKey := name == splicePK || name == spliceSK
		if name != "big" && (!isKey || g.rng.IntN(8) == 0) {
			targets = append(targets, name)
		}
	}

	return targets
}

// update draws an update of item: an UpdateExpression mostly, and now and
// then the legacy AttributeUpdates. It reports false for an expression that
// does not parse.
func (g *spliceGen) update(item value.Item) (spliceUpdate, bool) {
	if g.rng.IntN(4) == 0 {
		return g.legacy(item), true
	}

	text, names := g.expression(item)

	u, err := expression.ParseUpdate(text, expression.Substitutions{Names: names, Values: spliceValues()}, nil)
	if err != nil {
		return spliceUpdate{}, false
	}

	return spliceUpdate{text: text, read: func(value.Item) (*expression.UpdateExpr, error) { return u, nil }}, true
}

// legacy draws AttributeUpdates of one to four entries, each on its own
// attribute, translated as UpdateItem translates them once it has the item.
func (g *spliceGen) legacy(item value.Item) spliceUpdate {
	targets := g.targets(item)
	values := spliceValues()
	entries := make([]expression.LegacyUpdate, 0, 4)

	for _, i := range g.rng.Perm(len(targets))[:min(1+g.rng.IntN(4), len(targets))] {
		entry := expression.LegacyUpdate{Name: targets[i]}

		switch g.rng.IntN(4) {
		case 0, 1:
			entry.Action = expression.LegacyPut
			entry.Value = new(values[g.pick(slices.Sorted(maps.Keys(values)))])
		case 2:
			entry.Action = expression.LegacyAdd
			entry.Value = new(values[g.pick([]string{":n", spliceSS, spliceNS, spliceL})])
		default:
			entry.Action = expression.LegacyDelete
			if g.rng.IntN(2) == 0 {
				entry.Value = new(values[g.pick([]string{spliceSS, spliceNS})])
			}
		}

		entries = append(entries, entry)
	}

	slices.SortFunc(entries, func(a, b expression.LegacyUpdate) int { return strings.Compare(a.Name, b.Name) })

	text := append(make([]string, 0, 1+2*len(entries)), "AttributeUpdates")
	for _, e := range entries {
		text = append(text, e.Action, strconv.Quote(e.Name))
	}

	return spliceUpdate{text: strings.Join(text, " "), read: func(old value.Item) (*expression.UpdateExpr, error) {
		return expression.NewLegacyUpdate(entries, old), nil
	}}
}

// expression draws an update expression of one to four actions, each on its
// own attribute. Now and then a second action writes below the first one's
// attribute.
func (g *spliceGen) expression(item value.Item) (string, map[string]string) {
	targets := g.targets(item)
	names := map[string]string{"#m": g.pick(spliceNames), "#o": "other"}
	clauses := map[string][]string{}
	order := g.rng.Perm(len(targets))
	actions := min(1+g.rng.IntN(4), len(targets))

	for i := range actions {
		x, y := "#x"+strconv.Itoa(i), "#y"+strconv.Itoa(i)
		names[x], names[y] = targets[order[i]], g.pick(targets)
		v := g.pick([]string{":s", ":n", spliceSS, spliceNS, spliceL, ":m"})

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
		{clauseSet, x + " = list_append(" + x + ", " + spliceL + ")"},
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

// noUpdate is a read that returns no update.
func noUpdate(value.Item) (*expression.UpdateExpr, error) {
	return nil, nil //nolint:nilnil // read returns no update and no error
}

// requireSpliceMatches draws an item from seed and a chain of two or three
// updates of it, and applies each to the item stored, as Pack and as
// JSONPacker store it at first and as the previous splice left it after. It
// holds ApplyStoredSpliced to unpacking, applying and packing (ApplyStored
// and Pack): the same error, or bytes spliceMismatch finds nothing wrong
// with. Each update writes no attribute its WrittenNames does not list. It returns
// the count of updates spliced.
func requireSpliceMatches(t *testing.T, seed1, seed2 uint64) int {
	t.Helper()

	g := spliceGen{rng: rand.New(rand.NewPCG(seed1, seed2))}
	item, drawn := g.item()

	if value.ValidateItem(item) != nil {
		return 0
	}

	// Validation put the item's numbers, its key's among them, in canonical
	// form, and the key is read back from it.
	key := value.Item{}
	for name := range drawn {
		key[name] = item[name]
	}

	packed, err := value.Pack(item)
	require.NoError(t, err)

	var (
		o  value.Orderer
		jp value.JSONPacker
	)

	fromJSON, ok := jp.Pack(o.Append(nil, packed))
	require.True(t, ok, "JSONPacker refuses the json Orderer writes")

	stored := []value.Packed{packed, fromJSON}
	spliced := 0

	for range 2 + g.rng.IntN(2) {
		up, parsed := g.update(item)
		if !parsed {
			continue
		}

		var next value.Item

		for i := range stored {
			got, gotPacked, gotErr := expression.ApplyStoredSpliced(key, stored[i], noUpdate)
			require.NoError(t, gotErr)
			require.Equal(t, stored[i], gotPacked, "no update")
			require.True(t, value.EqualPacked(stored[i], got), "no update")

			want, wantErr := expression.ApplyStored(key, stored[i], up.read)
			_, gotPacked, gotErr = expression.ApplyStoredSpliced(key, stored[i], up.read)

			if wantErr != nil {
				require.EqualError(t, gotErr, wantErr.Error(), "update %q", up.text)

				continue
			}

			require.NoError(t, gotErr, "update %q", up.text)
			require.NotEmpty(t, gotPacked, "update %q", up.text)

			u, err := up.read(value.Unpack(stored[i]))
			require.NoError(t, err)
			require.Empty(t, changedOutside(value.Unpack(stored[i]), want, u.WrittenNames()),
				"attributes update %q changed that WrittenNames does not list", up.text)
			require.Empty(t, spliceMismatch(stored[i], want, gotPacked, u.WrittenNames()), "update %q", up.text)

			stored[i], next = gotPacked, want
			spliced++
		}

		if next != nil {
			item = next
		}
	}

	return spliced
}

// spliceMismatch is what is wrong with got, the bytes a splice of the
// attributes names into stored made, against want, the item that unpacking,
// applying and packing made: "" for nothing. got must be the bytes Pack
// writes for its own json, with nothing after it; hold the item want; have
// Pack's header for want and, each map's names in order, Pack's json; and
// begin with the members of stored that names does not list, in their order,
// which keeps the key where it was.
func spliceMismatch(stored value.Packed, want value.Item, got value.Packed, names []string) string {
	var (
		jp value.JSONPacker
		o  value.Orderer
	)

	repacked, ok := jp.Pack([]byte(got.JSON()))
	if !ok {
		return "the json is not what Pack writes"
	}

	if repacked != got {
		return "the header " + strconv.Quote(packedHeader(got)) +
			" is not the json's, " + strconv.Quote(packedHeader(repacked))
	}

	wantPacked, err := value.Pack(want)
	if err != nil {
		return err.Error()
	}

	if packedHeader(wantPacked) != packedHeader(got) {
		return "the header " + strconv.Quote(packedHeader(got)) + " is not Pack's, " + strconv.Quote(packedHeader(wantPacked))
	}

	if string(o.Append(nil, wantPacked)) != string(o.Append(nil, got)) {
		return "the ordered json is not Pack's"
	}

	if !value.EqualPacked(got, want) {
		return "the item is not the one applied"
	}

	kept := slices.DeleteFunc(memberNames(stored), func(name string) bool {
		_, written := slices.BinarySearch(names, name)

		return written
	})

	if members := memberNames(got); len(members) < len(kept) || !slices.Equal(members[:len(kept)], kept) {
		return "the members kept moved: " + strings.Join(members, ",")
	}

	return ""
}

// memberNames is the names of p's top-level members, in the order its json
// holds them.
func memberNames(p value.Packed) []string {
	dec := json.NewDecoder(strings.NewReader(p.JSON()))

	_, _ = dec.Token() // {

	var names []string

	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil
		}

		name, _ := tok.(string)

		var raw json.RawMessage
		if dec.Decode(&raw) != nil {
			return nil
		}

		names = append(names, name)
	}

	return names
}

// changedOutside is the top-level attributes of old and item that differ,
// held by one and not the other or holding different values, and that names
// does not list.
func changedOutside(old, item value.Item, names []string) []string {
	var out []string

	for name := range mergeKeys(old, item) {
		if _, listed := slices.BinarySearch(names, name); listed {
			continue
		}

		a, inOld := old[name]
		b, inItem := item[name]

		if inOld != inItem || (inOld && !value.Equal(&a, &b)) {
			out = append(out, name)
		}
	}

	slices.Sort(out)

	return out
}

// mergeKeys is a map holding the names of a and of b.
func mergeKeys(a, b value.Item) value.Item {
	out := maps.Clone(a)
	if out == nil {
		out = value.Item{}
	}

	maps.Copy(out, b)

	return out
}

// packedHeader is the bytes of p before its json: the item's size, the count
// of its S and N values and the count of its attributes.
func packedHeader(p value.Packed) string {
	return string(p)[:len(p)-len(p.JSON())]
}

// Test_Splice_MatchesPack is the differential property over many draws: an
// update spliced into the stored item, and into the bytes an earlier splice
// left, packs it as unpacking, applying and packing does. Many draws make an
// update that fails or does not parse, so the count of updates spliced is
// held too, lest the property hold of none.
func Test_Splice_MatchesPack(t *testing.T) {
	t.Parallel()

	const draws = 4000

	spliced := 0
	for i := range uint64(draws) {
		spliced += requireSpliceMatches(t, i, 0x5eed)
	}

	t.Logf("%d updates spliced over %d draws", spliced, draws)
	require.Greater(t, spliced, draws, "updates spliced")
}

// FuzzSplice is the property of Test_Splice_MatchesPack over any seed.
func FuzzSplice(f *testing.F) {
	for i := range uint64(8) {
		f.Add(i, uint64(1))
	}

	f.Fuzz(func(t *testing.T, seed1, seed2 uint64) { requireSpliceMatches(t, seed1, seed2) })
}

// spliceStored is the item the tests below update, packed from json written
// as Pack writes it, so its members keep this order: the two key attributes
// first, then the rest.
const spliceStored = `{"pk":{"S":"k"},"sk":{"N":"1"},"a":{"N":"1"},"b":{"S":"x"},` +
	`"m":{"M":{"x":{"N":"1"}}},"s":{"SS":["p","q"]},"l":{"L":[{"N":"1"}]}}`

// storedInOrder packs spliceStored, keeping its order.
func storedInOrder(t *testing.T) (value.Packed, value.Item) {
	t.Helper()

	var jp value.JSONPacker

	stored, ok := jp.Pack([]byte(spliceStored))
	require.True(t, ok)

	return stored, value.Item{"pk": {S: new("k")}, "sk": {N: new("1")}}
}

// Test_ApplyStoredSpliced_keysStay: a splice leaves the key attributes where
// the stored item has them, ahead of the members the update wrote, so the
// table's key reads still find them first.
func Test_ApplyStoredSpliced_keysStay(t *testing.T) {
	t.Parallel()

	stored, key := storedInOrder(t)

	u, err := expression.ParseUpdate("SET a = :n REMOVE b", expression.Substitutions{Values: spliceValues()}, nil)
	require.NoError(t, err)

	read := func(value.Item) (*expression.UpdateExpr, error) { return u, nil }

	want, err := expression.ApplyStored(key, stored, read)
	require.NoError(t, err)

	_, got, err := expression.ApplyStoredSpliced(key, stored, read)
	require.NoError(t, err)
	require.Equal(t, []string{"pk", "sk", "m", "s", "l", "a"}, memberNames(got))
	require.Empty(t, spliceMismatch(stored, want, got, u.WrittenNames()))
}

// Test_WrittenNames_listsEveryChange: each kind of action, of an UpdateExpression
// and of the legacy AttributeUpdates, changes no top-level attribute its
// WrittenNames does not list, and the splice of what it wrote is the item packed.
func Test_WrittenNames_listsEveryChange(t *testing.T) {
	t.Parallel()

	values := spliceValues()

	tests := []struct {
		name   string
		update string
		legacy []expression.LegacyUpdate
	}{
		{name: "set", update: "SET a = :s"},
		{name: "set a new attribute from another", update: "SET z = m"},
		{name: "set below a map", update: "SET m.y = :n"},
		{name: "set a list element", update: "SET l[0] = :s"},
		{name: "list_append", update: "SET l = list_append(l, :l)"},
		{name: "if_not_exists", update: "SET z = if_not_exists(z, :n)"},
		{name: "arithmetic", update: "SET a = a + :n"},
		{name: "remove", update: clauseRemove + " b"},
		{name: "remove below a map", update: "REMOVE m.x"},
		{name: "remove a list element", update: "REMOVE l[0]"},
		{name: "add to a number", update: "ADD a :n"},
		{name: "add to a set", update: "ADD s :ss"},
		{name: "delete from a set", update: "DELETE s :ss"},
		{name: "every clause", update: "SET a = :s, m.y = :n REMOVE b ADD s :ss, z :n"},
		{name: "legacy put", legacy: []expression.LegacyUpdate{
			{Name: "a", Action: expression.LegacyPut, Value: new(values[":m"])},
		}},
		{name: "legacy add to a list", legacy: []expression.LegacyUpdate{
			{Name: "l", Action: expression.LegacyAdd, Value: new(values[spliceL])},
			{Name: "z", Action: expression.LegacyAdd, Value: new(values[spliceL])},
		}},
		{name: "legacy delete", legacy: []expression.LegacyUpdate{
			{Name: "b", Action: expression.LegacyDelete},
			{Name: "s", Action: expression.LegacyDelete, Value: new(values[spliceSS])},
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stored, key := storedInOrder(t)

			read := func(old value.Item) (*expression.UpdateExpr, error) {
				if tt.legacy != nil {
					return expression.NewLegacyUpdate(tt.legacy, old), nil
				}

				return expression.ParseUpdate(tt.update, expression.Substitutions{Values: values}, nil)
			}

			want, err := expression.ApplyStored(key, stored, read)
			require.NoError(t, err)

			u, err := read(value.Unpack(stored))
			require.NoError(t, err)
			require.Empty(t, changedOutside(value.Unpack(stored), want, u.WrittenNames()))

			_, got, err := expression.ApplyStoredSpliced(key, stored, read)
			require.NoError(t, err)
			require.Empty(t, spliceMismatch(stored, want, got, u.WrittenNames()))
		})
	}
}

// Test_SpliceChecked_countGuard: a splice handed names that leave out an
// attribute the update added or removed holds a different count of
// attributes than the item, and is refused for the caller to pack. One that
// leaves out an attribute whose value the update changed passes the count,
// and only spliceMismatch finds it, which is why WrittenNames is held to every
// change above.
func Test_SpliceChecked_countGuard(t *testing.T) {
	t.Parallel()

	stored, _ := storedInOrder(t)
	old := value.Unpack(stored)

	tests := []struct {
		name       string
		giveNames  []string
		giveChange func(item value.Item)
		wantGuard  bool
	}{
		{
			name:       "every name listed",
			giveNames:  []string{"b", "z"},
			giveChange: func(item value.Item) { item["b"], item["z"] = api.AttributeValue{N: new("2")}, item["a"] },
		},
		{
			name:       "an added attribute left out",
			giveNames:  []string{"b"},
			giveChange: func(item value.Item) { item["b"], item["z"] = api.AttributeValue{N: new("2")}, item["a"] },
			wantGuard:  true,
		},
		{
			name:       "a removed attribute left out",
			giveNames:  []string{"a"},
			giveChange: func(item value.Item) { item["a"] = api.AttributeValue{N: new("2")}; delete(item, "b") },
			wantGuard:  true,
		},
		{
			name:       "a changed attribute left out",
			giveNames:  []string{"b"},
			giveChange: func(item value.Item) { item["a"], item["b"] = api.AttributeValue{N: new("2")}, item["a"] },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			item := value.Unpack(stored)
			tt.giveChange(item)

			got, err := expression.SpliceChecked(stored, tt.giveNames, item)
			require.NoError(t, err)

			if tt.wantGuard {
				require.Empty(t, got, "the count guard refuses the splice")

				return
			}

			require.NotEmpty(t, got)

			if changed := changedOutside(old, item, tt.giveNames); len(changed) > 0 {
				require.NotEmpty(t, spliceMismatch(stored, item, got, tt.giveNames),
					"a splice that left out %v matches", changed)

				return
			}

			require.Empty(t, spliceMismatch(stored, item, got, tt.giveNames))
		})
	}
}

// Test_ApplyStoredPacked_spliceWorth: an update whose old values and new ones
// come to less than half the stored item is spliced into it, and one whose
// come to half or more is left for the caller to pack, a REMOVE of a large
// attribute among them, as is an update of no stored item.
func Test_ApplyStoredPacked_spliceWorth(t *testing.T) {
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
		{name: "a remove of the large string", giveUpdate: "REMOVE d", wantSpliced: false},
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
				require.Empty(t, spliceMismatch(stored, got, packed, u.WrittenNames()))
			}

			_, packed, err = expression.ApplyStoredPacked(key, "", read)
			require.NoError(t, err)
			require.Empty(t, packed, "no stored item")
		})
	}
}
