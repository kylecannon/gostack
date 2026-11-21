package value_test

import (
	"cmp"
	"math/rand/v2"
	"strings"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
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
