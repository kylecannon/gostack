package value

import (
	"unsafe"

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

// maxNumberKey is the longest key of a number: the sign and the exponent, 38
// digits two to a byte, and the end.
const maxNumberKey = 2 + maxDigits/2 + 1

// sortExpBias moves the exponent of a number, from -129 to 126 as ParseNumber
// admits it, onto 0 to 255, so it takes one byte.
const sortExpBias = 129

// bitsPerDigit is the width of one packed decimal digit; two share a byte.
const bitsPerDigit = 4

// SortKeyUntyped encodes a scalar key value as a string whose byte order is
// the order of Compare for two values of one type, which is all that a table or
// an index orders: the type of its range key is fixed by its schema, so no
// letter tells it. A string is its own string, shared and not copied. A binary
// is its own bytes, shared as a string. A number is its sign, then its
// exponent and its digits packed two to a byte, the whole of a negative number
// inverted so that the larger magnitude sorts first and closed by a byte
// above any digit pair. A value that is not an S, N or B has the empty key.
//
// A partition of many items keeps this string in each entry, in place of the
// value, which would hold the whole of an AttributeValue. A string or binary
// key then costs an entry no more than the pointer it was before, however long
// it is. The numbers must be canonical, as Validate leaves them.
func SortKeyUntyped(v *api.AttributeValue) string {
	switch {
	case v.S != nil:
		return *v.S
	case v.B != nil:
		if len(v.B) == 0 {
			return ""
		}

		// The key and the binary share their bytes, which cannot change under
		// the key: a binary is stored in an item, a stored item is never
		// written to, and the key of a lookup is dropped with the request.
		return unsafe.String(unsafe.SliceData(v.B), len(v.B)) //nolint:gosec // G103: see above
	case v.N != nil:
		var buf [maxNumberKey]byte

		return string(appendNumberKey(buf[:0], *v.N))
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
