package value_test

import (
	"math/rand/v2"
	"strings"
	"testing"
	"unsafe"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// untypedOrder is -1, 0 or 1: how the untyped sort keys of a and b, values of
// one type, order.
func untypedOrder(a, b *api.AttributeValue) int {
	return strings.Compare(value.SortKeyUntyped(a), value.SortKeyUntyped(b))
}

func Test_SortKeyUntyped_ordersRandomKeysAsCompare(t *testing.T) {
	kinds := []struct {
		name string
		gen  func(*rand.Rand) api.AttributeValue
	}{{"S", randomString}, {"N", randomNumber}, {"B", randomBinary}}

	for _, kind := range kinds {
		t.Run(kind.name, func(t *testing.T) {
			r, gen := rand.New(rand.NewPCG(408, 408)), kind.gen

			for range 20000 {
				a, b := gen(r), gen(r)

				require.Equal(t, valueOrder(&a, &b), untypedOrder(&a, &b), "%+v against %+v", a, b)
			}
		})
	}
}

func Test_SortKeyUntyped_ordersNumberFormsAsCompare(t *testing.T) {
	forms := make([]api.AttributeValue, 0, len(numberForms))

	for _, text := range numberForms {
		n, err := value.ParseNumber(text)
		require.NoError(t, err, text)
		forms = append(forms, numv(n.String()))
	}

	for _, a := range forms {
		for _, b := range forms {
			assert.Equal(t, valueOrder(&a, &b), untypedOrder(&a, &b), "%s against %s", *a.N, *b.N)
		}
	}
}

func Test_SortKeyUntyped_stringOrderIsByteOrder(t *testing.T) {
	// DynamoDB orders a string by its UTF-8 bytes, not by its code points
	// as UTF-16 would: U+FF5E (EF BD 9E) sorts before U+10000 (F0 90 80 80).
	a, b := sv("～"), sv("\U00010000")

	assert.Negative(t, untypedOrder(&a, &b))
	assert.Equal(t, valueOrder(&a, &b), untypedOrder(&a, &b))

	// Case counts: "B" (0x42) sorts before "a" (0x61), and "b" is not "B".
	upper, lower := sv("B"), sv("a")

	assert.Negative(t, untypedOrder(&upper, &lower))
	assert.Equal(t, valueOrder(&upper, &lower), untypedOrder(&upper, &lower))

	folded := sv("b")

	assert.NotEqual(t, value.SortKeyUntyped(&upper), value.SortKeyUntyped(&folded))

	// A string is its bytes: a prefix sorts first.
	short, long := sv("ab"), sv("ab\x00")

	assert.Negative(t, untypedOrder(&short, &long))
}

func Test_SortKeyUntyped_binaryOrderIsUnsignedByteOrder(t *testing.T) {
	low, high := bv([]byte{0x7f}), bv([]byte{0x80})

	assert.Negative(t, untypedOrder(&low, &high))
	assert.Equal(t, valueOrder(&low, &high), untypedOrder(&low, &high))
}

func Test_SortKeyUntyped_sharesTheValuesBytes(t *testing.T) {
	str := sv(strings.Repeat("k", 1000))
	bin := bv([]byte(strings.Repeat("k", 1000)))

	assert.Same(t, unsafe.StringData(*str.S), unsafe.StringData(value.SortKeyUntyped(&str)))
	assert.Same(t, unsafe.SliceData(bin.B), unsafe.StringData(value.SortKeyUntyped(&bin)))

	assert.Zero(t, testing.AllocsPerRun(100, func() { _ = value.SortKeyUntyped(&str) }), "a string key copies nothing")
	assert.Zero(t, testing.AllocsPerRun(100, func() { _ = value.SortKeyUntyped(&bin) }), "a binary key copies nothing")
}

func Test_SortKeyUntyped_scalarsAndOthers(t *testing.T) {
	assert.Equal(t, "ab", value.SortKeyUntyped(new(sv("ab"))))
	assert.Equal(t, "\x01\x02", value.SortKeyUntyped(new(bv([]byte{1, 2}))))
	assert.Equal(t, string([]byte{0x01}), value.SortKeyUntyped(new(numv("0"))), "zero is one byte")
	assert.Empty(t, value.SortKeyUntyped(new(bv(nil))))
	assert.Empty(t, value.SortKeyUntyped(&api.AttributeValue{}))
}

func Test_SortKey_isTheUntypedKeyUnderItsLetter(t *testing.T) {
	for _, v := range []api.AttributeValue{sv("ab"), bv([]byte{1, 2}), numv("12.5"), numv("-3")} {
		assert.Equal(t, value.Type(&v)+value.SortKeyUntyped(&v), value.SortKey(&v), "%+v", v)
	}
}
