package value_test

import (
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

// Test_EqualPacked: a packed item equals an item exactly when EqualItems
// says the two items do. The rows change one thing each, at the top or below,
// in each type; sets in another order and maps in another order are equal.
func Test_EqualPacked(t *testing.T) {
	t.Parallel()

	long := []byte(strings.Repeat("0123456789", 100))
	longOther := slices.Clone(long)
	longOther[len(longOther)-1] = 'x'

	base := func() value.Item {
		return value.Item{
			"id":         {S: new("k")},
			`q"uo\te`:    {S: new("é\n\"")},
			"n":          {N: new("12.5")},
			"b":          {B: slices.Clone(long)},
			"ss":         {SS: []string{"a", "b", "c"}},
			"ns":         {NS: []string{"1", "2"}},
			"bs":         {BS: [][]byte{[]byte("x"), []byte("y")}},
			"m":          {M: value.Item{"x": {N: new("1")}, "l": {L: []api.AttributeValue{{S: new("e")}, {NULL: new(true)}}}}},
			"null":       {NULL: new(true)},
			"bool":       {BOOL: new(false)},
			"emptyList":  {L: []api.AttributeValue{}},
			"emptyMap":   {M: value.Item{}},
			"emptyBytes": {B: []byte{}},
		}
	}

	packed, err := value.Pack(base())
	require.NoError(t, err)

	for name, change := range map[string]func(value.Item){
		"same":              func(value.Item) {},
		"set order":         func(it value.Item) { it["ss"] = api.AttributeValue{SS: []string{"c", "a", "b"}} },
		"binary set order":  func(it value.Item) { it["bs"] = api.AttributeValue{BS: [][]byte{[]byte("y"), []byte("x")}} },
		"string":            func(it value.Item) { it["id"] = api.AttributeValue{S: new("j")} },
		"escaped string":    func(it value.Item) { it[`q"uo\te`] = api.AttributeValue{S: new("é\n'")} },
		"number":            func(it value.Item) { it["n"] = api.AttributeValue{N: new("12.6")} },
		"type":              func(it value.Item) { it["n"] = api.AttributeValue{S: new("12.5")} },
		"binary end":        func(it value.Item) { it["b"] = api.AttributeValue{B: longOther} },
		"binary shorter":    func(it value.Item) { it["b"] = api.AttributeValue{B: long[:len(long)-1]} },
		"empty binary":      func(it value.Item) { it["emptyBytes"] = api.AttributeValue{B: []byte{0}} },
		"set member":        func(it value.Item) { it["ns"] = api.AttributeValue{NS: []string{"1", "3"}} },
		"binary set member": func(it value.Item) { it["bs"] = api.AttributeValue{BS: [][]byte{[]byte("x"), []byte("z")}} },
		"nested number": func(it value.Item) {
			it["m"].M["x"] = api.AttributeValue{N: new("2")}
		},
		"nested list element": func(it value.Item) {
			it["m"].M["l"] = api.AttributeValue{L: []api.AttributeValue{{S: new("e")}, {BOOL: new(true)}}}
		},
		"list longer": func(it value.Item) {
			it["m"].M["l"] = api.AttributeValue{L: []api.AttributeValue{{S: new("e")}, {NULL: new(true)}, {N: new("1")}}}
		},
		"list shorter": func(it value.Item) {
			it["m"].M["l"] = api.AttributeValue{L: []api.AttributeValue{{S: new("e")}}}
		},
		"nested member added": func(it value.Item) { it["m"].M["y"] = api.AttributeValue{N: new("1")} },
		"nested member gone":  func(it value.Item) { delete(it["m"].M, "x") },
		"empty list filled": func(it value.Item) {
			it["emptyList"] = api.AttributeValue{L: []api.AttributeValue{{N: new("1")}}}
		},
		"empty map filled": func(it value.Item) { it["emptyMap"] = api.AttributeValue{M: value.Item{"a": {N: new("1")}}} },
		"bool":             func(it value.Item) { it["bool"] = api.AttributeValue{BOOL: new(true)} },
		"attribute added":  func(it value.Item) { it["z"] = api.AttributeValue{N: new("1")} },
		"attribute gone":   func(it value.Item) { delete(it, "null") },
		"attribute renamed": func(it value.Item) {
			it["nul"] = it["null"]
			delete(it, "null")
		},
	} {
		item := base()
		change(item)

		require.Equal(t, value.EqualItems(value.Unpack(packed), item), value.EqualPacked(packed, item), name)
		require.Equal(t, name == "same" || strings.Contains(name, "order"), value.EqualPacked(packed, item), name)
	}

	require.True(t, value.EqualPacked("", nil))
	require.False(t, value.EqualPacked("", base()))
	require.False(t, value.EqualPacked(packed, nil))
}

// Test_EqualPacked_random: on random items, and on each item against itself
// and against a copy with one value changed, EqualPacked answers as
// EqualItems does on the unpacked item.
func Test_EqualPacked_random(t *testing.T) {
	t.Parallel()

	r := rand.New(rand.NewPCG(7, 11))

	for range 2000 {
		a, b := randomPackItem(r), randomPackItem(r)

		packed, err := value.Pack(a)
		require.NoError(t, err)

		require.True(t, value.EqualPacked(packed, a))
		require.Equal(t, value.EqualItems(a, b), value.EqualPacked(packed, b))

		for name := range a {
			changed := maps.Clone(a)

			changed[name] = randomPackValue(r, 2)
			require.Equal(t, value.EqualItems(a, changed), value.EqualPacked(packed, changed))

			break
		}
	}
}

// Test_EqualPacked_allocates: comparing a packed item of strings, numbers,
// binaries, maps and lists allocates nothing, whether the items are equal or
// differ at the end.
func Test_EqualPacked_allocates(t *testing.T) {
	item := value.Item{
		"id": {S: new("k")},
		"n":  {N: new("1")},
		"b":  {B: []byte(strings.Repeat("y", 4096))},
		"m":  {M: value.Item{"x": {N: new("1")}}},
		"l":  {L: []api.AttributeValue{{M: value.Item{"s": {S: new("e")}}}, {BOOL: new(true)}}},
	}

	packed, err := value.Pack(item)
	require.NoError(t, err)

	other := maps.Clone(item)

	differs := []byte(strings.Repeat("y", 4096))
	differs[4095] = 'z'
	other["b"] = api.AttributeValue{B: differs}

	require.Zero(t, testing.AllocsPerRun(100, func() {
		if !value.EqualPacked(packed, item) {
			t.Fatal("not equal to itself")
		}
	}))
	require.Zero(t, testing.AllocsPerRun(100, func() {
		if value.EqualPacked(packed, other) {
			t.Fatal("equal to an item that differs")
		}
	}))
}
