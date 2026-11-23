package value_test

import (
	"maps"
	"math/rand/v2"
	"slices"
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

// Test_Packed_Number checks Number reads each attribute's number as Unpack
// does, for plain names, names with escapes and absent names, and that it
// allocates nothing for a plain name. It is not parallel: AllocsPerRun
// refuses a parallel test.
func Test_Packed_Number(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))

	for range 2000 {
		item := randomPackItem(r)
		item["ttl"] = randomPackValue(r, 1)
		item["é"] = api.AttributeValue{N: new(strconv.Itoa(r.IntN(100)))}

		p, err := value.Pack(item)
		if err != nil {
			t.Fatalf("Pack: %v", err)
		}

		for _, name := range append(slices.Collect(maps.Keys(item)), "absent", "a", `a"\`, "") {
			want, ok := item[name]
			wantN := ok && want.N != nil

			got, gotN := p.Number(name)
			if gotN != wantN || (wantN && got != *want.N) {
				t.Fatalf("Number(%q) = %q, %v; want %v", name, got, gotN, want)
			}
		}
	}

	p, err := value.Pack(value.Item{"pk": {S: new("k")}, "ttl": {N: new("5")}})
	if err != nil {
		t.Fatal(err)
	}

	if n := testing.AllocsPerRun(100, func() { p.Number("ttl") }); n != 0 {
		t.Fatalf("Number allocates %v times", n)
	}

	if _, ok := value.Packed("").Number("ttl"); ok {
		t.Fatal("no item has a number")
	}
}

func Test_Pack_noItem(t *testing.T) {
	t.Parallel()

	p, err := value.Pack(nil)
	if err != nil || p != "" || value.Unpack(p) != nil || p.Encoded() != nil || p.Size() != 0 {
		t.Fatalf("Pack(nil) = %q, %v", p, err)
	}
}
