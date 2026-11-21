package value

import (
	api "github.com/kylecannon/gostack/internal/api/dynamodb"
)

// The bytes of a sort key that tell a number's sign, in the order of the
// numbers they stand for, and the one that ends a negative number's digits.
const (
	sortNegative byte = 0x00
	sortZero     byte = 0x01
	sortPositive byte = 0x02
	sortEnd      byte = 0xFF
)

// maxNumberKey is the longest key of a number: the type's letter, the sign and
// the exponent, 38 digits two to a byte, and the end.
const maxNumberKey = 1 + 2 + maxDigits/2 + 1

// sortExpBias moves the exponent of a number, from -129 to 126 as ParseNumber
// admits it, onto 0 to 255, so it takes one byte.
const sortExpBias = 129

// bitsPerDigit is the width of one packed decimal digit; two share a byte.
const bitsPerDigit = 4

// SortKey encodes a scalar key value as a string whose byte order is the order
// of Compare for two values of one type: the type's letter, then the value. A
// string or binary follows as it is. A number follows as its sign, then its
// exponent and its digits packed two to a byte, the whole of a negative number
// inverted so that the larger magnitude sorts first and closed by a byte
// above any digit pair. Two values of different types order by their letters,
// as Compare orders them by name. A value that is not an S, N or B has the
// empty key.
//
// A partition of many items keeps this string in each entry, in place of the
// value, which would hold the whole of an AttributeValue. The numbers must be
// canonical, as Validate leaves them.
func SortKey(v *api.AttributeValue) string {
	switch {
	case v.S != nil:
		return TypeS + *v.S
	case v.B != nil:
		return TypeB + string(v.B)
	case v.N != nil:
		var buf [maxNumberKey]byte

		return string(appendNumberKey(append(buf[:0], TypeN...), *v.N))
	default:
		return ""
	}
}

// appendNumberKey appends the key of the number in text to dst. Text that is
// not a number is read as zero, as Compare reads it.
func appendNumberKey(dst []byte, text string) []byte {
	n, _ := ParseNumber(text)

	if n.Digits == "" {
		return append(dst, sortZero)
	}

	start := len(dst)

	sign := sortPositive
	if n.Neg {
		sign = sortNegative
	}

	// ParseNumber holds the exponent to what sortExpBias fits in a byte.
	dst = append(dst, sign, byte(n.Exp+len(n.Digits)+sortExpBias)) //nolint:gosec // G115: see above

	for i := 0; i < len(n.Digits); i += 2 {
		// A digit is stored as itself plus one, so that the nibble that pads
		// an odd count is below every digit and a shorter run sorts first.
		pair := (n.Digits[i] - '0' + 1) << bitsPerDigit
		if i+1 < len(n.Digits) {
			pair |= n.Digits[i+1] - '0' + 1
		}

		dst = append(dst, pair)
	}

	if !n.Neg {
		return dst
	}

	for i := start + 1; i < len(dst); i++ {
		dst[i] = ^dst[i]
	}

	return append(dst, sortEnd)
}

// SortKeyHasPrefix reports whether the sort key k is that of a string or a
// binary that begins with the one whose sort key is prefix, which is what
// begins_with asks of a range key. A number has no such prefix.
func SortKeyHasPrefix(k, prefix string) bool {
	if len(prefix) == 0 || prefix[0] == TypeN[0] || len(k) < len(prefix) {
		return false
	}

	return k[:len(prefix)] == prefix
}
