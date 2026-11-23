package value_test

import (
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
)

// randomPackValue is a random attribute value nested at most depth levels,
// its strings drawn to need json escapes.
func randomPackValue(r *rand.Rand, depth int) api.AttributeValue {
	pieces := []string{"a", "é", `"`, `\`, "\n", "\x01", " ", "<>&", "z"}
	str := func() string {
		var b strings.Builder
		for range r.IntN(6) {
			b.WriteString(pieces[r.IntN(len(pieces))])
		}

		return b.String()
	}
	num := func() string { return strconv.Itoa(r.IntN(2000) - 1000) }

	kind := r.IntN(10)
	if depth <= 0 && kind >= 6 {
		kind = r.IntN(6)
	}

	switch kind {
	case 0:
		return api.AttributeValue{S: new(str())}
	case 1:
		return api.AttributeValue{N: new(num())}
	case 2:
		b := []byte(str())

		return api.AttributeValue{B: b}
	case 3:
		return api.AttributeValue{SS: []string{str() + "1", str() + "2"}}
	case 4:
		return api.AttributeValue{NS: []string{"1", num() + "0.5"}}
	case 5:
		if r.IntN(2) == 0 {
			return api.AttributeValue{NULL: new(true)}
		}

		return api.AttributeValue{BOOL: new(r.IntN(2) == 0)}
	case 6:
		return api.AttributeValue{BS: [][]byte{[]byte(str() + "x"), {}}}
	case 7:
		m := map[string]api.AttributeValue{}
		for range r.IntN(4) {
			m[str()] = randomPackValue(r, depth-1)
		}

		return api.AttributeValue{M: m}
	default:
		l := []api.AttributeValue{}
		for range r.IntN(4) {
			l = append(l, randomPackValue(r, depth-1))
		}

		return api.AttributeValue{L: l}
	}
}

func randomPackItem(r *rand.Rand) value.Item {
	item := value.Item{"pk": {S: new("k" + strconv.Itoa(r.IntN(100)))}}
	for range r.IntN(30) {
		item["a"+strconv.Itoa(r.IntN(40))+`"\`] = randomPackValue(r, 3)
	}

	return item
}

// Test_Pack_roundTrips packs random items and checks that each unpacks to an
// equal item, that its json is what the typed encoder writes for the item, and
// that Attrs reads the named attributes alone.
func Test_Pack_roundTrips(t *testing.T) {
	t.Parallel()

	r := rand.New(rand.NewPCG(1, 2))

	for range 2000 {
		item := randomPackItem(r)

		p, err := value.Pack(item)
		if err != nil {
			t.Fatalf("Pack: %v", err)
		}

		if got := value.Unpack(p); !value.EqualItems(got, item) {
			t.Fatalf("Unpack(Pack(%v)) = %v", item, got)
		}

		if got := value.UnpackJSON(p.Encoded()); !value.EqualItems(got, item) {
			t.Fatalf("UnpackJSON = %v, want %v", got, item)
		}

		if p.Size() != value.ItemSize(item) {
			t.Fatalf("Size = %d, want %d", p.Size(), value.ItemSize(item))
		}

		typed, err := (&api.GetItemOutput{Item: value.UnpackJSON(p.Encoded())}).AppendJSON(nil)
		if err != nil {
			t.Fatal(err)
		}

		if len(typed) != len(p.JSON())+len(`{"Item":}`) {
			t.Fatalf("typed encoding is %d bytes, packed %d", len(typed), len(p.JSON()))
		}

		var names []string
		for name := range item {
			if r.IntN(3) == 0 {
				names = append(names, name)
			}
		}

		names = append(names, "absent")

		part := p.Attrs(names)
		for _, name := range names {
			want, ok := item[name]
			got, gotOK := part[name]

			if ok != gotOK || (ok && !value.Equal(&got, &want)) {
				t.Fatalf("Attrs(%q) = %v, want %v", name, got, want)
			}
		}

		if len(part) != len(names)-1 {
			t.Fatalf("Attrs read %d attributes, want %d", len(part), len(names)-1)
		}
	}
}

// Test_Decoder_reuse decodes random items one after another through one
// Decoder: Peek and View answer what Attrs does though their maps held the
// item before, a value View answered stays as it was after later calls, and
// ViewSelect keeps of each map only the members its paths name, though each
// Selection is used again on items whose maps hold other members.
func Test_Decoder_reuse(t *testing.T) {
	t.Parallel()

	r := rand.New(rand.NewPCG(3, 4))

	var dec value.Decoder

	type kept struct {
		got, want api.AttributeValue
	}

	const rounds = 2000

	held := make([]kept, 0, rounds)

	// A read uses one Selection for every item, so the maps ViewSelect keeps
	// part of are reused from item to item.
	type selection struct {
		sel *value.Selection
		ref selRef
	}

	selections := make([]selection, 6)
	for i := range selections {
		selections[i].sel, selections[i].ref = randomSelection(r)
	}

	// pruned counts the maps ViewSelect kept part of.
	pruned := 0

	for round := range rounds {
		item := randomPackItem(r)

		p, err := value.Pack(item)
		if err != nil {
			t.Fatal(err)
		}

		names := []string{"absent"}
		for i := range 40 {
			if r.IntN(4) == 0 {
				names = append(names, "a"+strconv.Itoa(i)+`"\`)
			}
		}

		want := p.Attrs(names)

		for _, read := range []func(value.Packed, []string) value.Item{dec.Peek, dec.View, dec.Item} {
			if got := read(p, names); !value.EqualItems(got, want) {
				t.Fatalf("round %d: decoded %v, want %v", round, got, want)
			}
		}

		for name, v := range dec.View(p, names) {
			held = append(held, kept{v, want[name]})
		}

		s := selections[r.IntN(len(selections))]

		got, wantSel := dec.ViewSelect(p, s.sel), s.ref.prune(item)
		if !value.EqualItems(got, wantSel) {
			t.Fatalf("round %d: ViewSelect = %v, want %v", round, got, wantSel)
		}

		for name, v := range got {
			if v.M != nil && len(v.M) < len(item[name].M) {
				pruned++
			}
		}

		if round%100 == 99 {
			dec.Release()
		}
	}

	for _, k := range held {
		if !value.Equal(&k.got, &k.want) {
			t.Fatalf("a value View answered changed to %v, want %v", k.got, k.want)
		}
	}

	if pruned < 50 {
		t.Fatalf("ViewSelect kept part of a map %d times; the items give it too few to check", pruned)
	}
}

// selRef is what a Selection should keep: each name, nil for the whole value.
type selRef map[string]selRef

// randomSelection is a Selection of up to four paths of up to three names, and
// what it keeps. The names below the top are short names randomPackValue's
// maps often hold.
func randomSelection(r *rand.Rand) (*value.Selection, selRef) {
	sel, ref := &value.Selection{}, selRef{}
	members := []string{"", "a", "z", "é", `"`, `\`}

	for range 1 + r.IntN(4) {
		path := make([]string, 1+r.IntN(3))

		path[0] = "a" + strconv.Itoa(r.IntN(40)) + `"\`
		for i := 1; i < len(path); i++ {
			path[i] = members[r.IntN(len(members))]
		}

		sel.Add(path)
		ref.add(path)
	}

	return sel, ref
}

func (s selRef) add(path []string) {
	below, ok := s[path[0]]

	switch {
	case ok && below == nil:
	case len(path) == 1:
		s[path[0]] = nil
	default:
		if !ok {
			below = selRef{}
			s[path[0]] = below
		}

		below.add(path[1:])
	}
}

// prune is what of item s keeps: a value that is not a map is kept whole.
func (s selRef) prune(item value.Item) value.Item {
	out := value.Item{}

	for name, below := range s {
		v, ok := item[name]
		if !ok {
			continue
		}

		if below != nil && v.M != nil {
			v = api.AttributeValue{M: below.prune(v.M)}
		}

		out[name] = v
	}

	return out
}

func Test_Pack_noItem(t *testing.T) {
	t.Parallel()

	p, err := value.Pack(nil)
	if err != nil || p != "" || value.Unpack(p) != nil || p.Encoded() != nil || p.Size() != 0 {
		t.Fatalf("Pack(nil) = %q, %v", p, err)
	}
}
