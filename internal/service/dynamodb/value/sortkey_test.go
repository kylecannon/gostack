package value_test

import (
	"cmp"
	"math/rand/v2"
	"strings"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sv(v string) api.AttributeValue   { return api.AttributeValue{S: &v} }
func bv(v []byte) api.AttributeValue   { return api.AttributeValue{B: v} }
func numv(v string) api.AttributeValue { return api.AttributeValue{N: &v} }

// numberForms are numbers at the edges of DynamoDB's: signs and zeros, fractions,
// digits that are prefixes of each other, 38 digits and the limits of the
// exponent, each as the canonical text Validate leaves.
var numberForms = []string{
	"0", "1", "-1", "2", "-2", "9", "-9", "10", "-10", "11", "100", "-100", "101", "1001",
	"12", "123", "1234", "-12", "-123", "-1234", "120", "-120", "0.1", "-0.1", "0.01", "-0.01", "0.25", "-0.25",
	"2.5", "-2.5", "9.99", "-9.99", "99", "-99", "0.000001", "-0.000001",
	"12345678901234567890123456789012345678", "-12345678901234567890123456789012345678",
	"99999999999999999999999999999999999999", "-99999999999999999999999999999999999999",
	"1234567890123456789012345678901234567.8", "0.12345678901234567890123456789012345678",
	"1" + strings.Repeat("0", 125), "-1" + strings.Repeat("0", 125),
	"9.9999999999999999999999999999999999999" + strings.Repeat("0", 89),
	"0." + strings.Repeat("0", 129) + "1", "-0." + strings.Repeat("0", 129) + "1",
	"0." + strings.Repeat("0", 129) + "99999999999999999999999999999999999999",
}

// valueOrder is -1, 0 or 1: how Compare orders a and b.
func valueOrder(a, b *api.AttributeValue) int {
	return cmp.Compare(value.Compare(a, b), 0)
}

// keyOrder is -1, 0 or 1: how the sort keys of a and b order.
func keyOrder(a, b *api.AttributeValue) int {
	return strings.Compare(value.SortKey(a), value.SortKey(b))
}

func Test_SortKey_ordersAsCompare(t *testing.T) {
	forms := make([]api.AttributeValue, 0, len(numberForms))

	for _, text := range numberForms {
		n, err := value.ParseNumber(text)
		require.NoError(t, err, text)
		forms = append(forms, numv(n.String()))
	}

	for _, a := range forms {
		for _, b := range forms {
			assert.Equal(t, valueOrder(&a, &b), keyOrder(&a, &b), "%s against %s", *a.N, *b.N)
		}
	}
}

// randomNumber is a canonical number of up to 38 digits drawn from a small
// alphabet, so that digit runs are often prefixes of each other, with a random
// point and sign, now and then at the limits of the exponent.
func randomNumber(r *rand.Rand) api.AttributeValue {
	for {
		var b strings.Builder

		if r.IntN(2) == 0 {
			b.WriteByte('-')
		}

		digits := 1 + r.IntN(38)
		alphabet := [...]string{"0123456789", "01", "09", "5"}[r.IntN(4)]

		for range digits {
			b.WriteByte(alphabet[r.IntN(len(alphabet))])
		}

		switch r.IntN(4) {
		case 0:
			b.WriteString("E" + []string{"-130", "-100", "-5", "0", "5", "88", "125"}[r.IntN(7)])
		case 1:
			text := b.String()
			b.Reset()
			b.WriteString(text[:1+r.IntN(len(text))] + "." + text[1:])
		default:
		}

		n, err := value.ParseNumber(b.String())
		if err != nil {
			continue
		}

		return numv(n.String())
	}
}

func randomString(r *rand.Rand) api.AttributeValue {
	alphabet := []string{"ab", "abc\x00\xff", "\x00\xff", "éß"}[r.IntN(4)]

	var b strings.Builder

	for range 1 + r.IntN(6) {
		b.WriteByte(alphabet[r.IntN(len(alphabet))])
	}

	// A string is UTF-8 on the wire; keep the ones that are.
	return sv(strings.ToValidUTF8(b.String(), "?"))
}

func randomBinary(r *rand.Rand) api.AttributeValue {
	b := make([]byte, 1+r.IntN(5))
	for i := range b {
		b[i] = []byte{0x00, 0x01, 0x7f, 0x80, 0xfe, 0xff}[r.IntN(6)]
	}

	return bv(b)
}

func Test_SortKey_ordersRandomKeysAsCompare(t *testing.T) {
	kinds := []struct {
		name string
		gen  func(*rand.Rand) api.AttributeValue
	}{{"S", randomString}, {"N", randomNumber}, {"B", randomBinary}}

	for _, kind := range kinds {
		t.Run(kind.name, func(t *testing.T) {
			// Each kind draws from its own sequence, so a failure replays.
			r, gen := rand.New(rand.NewPCG(405, 405)), kind.gen

			for range 20000 {
				a, b := gen(r), gen(r)

				want := valueOrder(&a, &b)
				require.Equal(t, want, keyOrder(&a, &b), "%+v against %+v", a, b)

				if want == 0 {
					assert.Equal(t, value.SortKey(&a), value.SortKey(&b))
				}
			}
		})
	}
}

func Test_SortKey_typesOrderByName(t *testing.T) {
	a, b, c := sv("x"), numv("1"), bv([]byte("x"))

	for _, pair := range [][2]api.AttributeValue{{a, b}, {b, c}, {a, c}, {b, a}, {c, b}, {c, a}} {
		x, y := pair[0], pair[1]
		assert.Equal(t, valueOrder(&x, &y), keyOrder(&x, &y))
	}

	assert.Empty(t, value.SortKey(&api.AttributeValue{}))
	assert.NotEmpty(t, value.SortKey(new(sv(""))))
}

func Test_SortKeyHasPrefix(t *testing.T) {
	for _, tt := range []struct {
		give, prefix api.AttributeValue
		want         bool
	}{
		{sv("abc"), sv("ab"), true},
		{sv("abc"), sv("abc"), true},
		{sv("ab"), sv("abc"), false},
		{sv("abc"), sv(""), true},
		{sv("abc"), sv("b"), false},
		{bv([]byte{1, 2, 3}), bv([]byte{1, 2}), true},
		{bv([]byte{1, 2}), bv([]byte{1, 2, 3}), false},
		{sv("abc"), bv([]byte("ab")), false},
		{numv("12"), numv("1"), false},
		{numv("12"), numv("12"), false},
	} {
		assert.Equal(t, tt.want, value.SortKeyHasPrefix(value.SortKey(&tt.give), value.SortKey(&tt.prefix)),
			"%+v begins with %+v", tt.give, tt.prefix)
	}
}
