package value

import "strings"

// The limits of a DynamoDB number: 38 significant digits, and a magnitude from
// 1E-130 up to, but not including, 1E126.
const (
	maxDigits = 38
	// maxLeading is the largest power of ten the leading digit may stand at.
	maxLeading = 125
	// minLeading is the smallest.
	minLeading = -130
	// signAndPoint is the room String needs beyond the digits and zeros: a sign
	// and "0.".
	signAndPoint = 3
	// expCap bounds the exponent read from the text, far past both limits, so
	// a long exponent cannot overflow an int.
	expCap = 1_000_000
)

// The messages DynamoDB Local answers for a number it refuses, as LocalStack's
// image returns them.
const (
	msgNotANumber = "A value provided cannot be converted into a number"
	msgPrecision  = "DynamoDB only supports precision up to 38 digits"
	msgOverflow   = "Number overflow. Attempting to store a number with magnitude larger than supported range"
	msgUnderflow  = "Number underflow. Attempting to store a number with magnitude smaller than supported range"
)

// Number is a parsed DynamoDB number: its value is Digits times ten to the
// power Exp, negated when Neg. Digits holds the significant digits with no
// leading or trailing zero, and is empty for zero, which is never negative.
type Number struct {
	Neg    bool
	Digits string
	Exp    int
}

// ParseNumber reads a number in DynamoDB's text form: an optional sign, digits
// with an optional decimal point, and an optional exponent. It refuses
// whitespace, a number with more than 38 significant digits, and one whose
// magnitude is outside the supported range, with DynamoDB's message.
func ParseNumber(s string) (Number, error) {
	parts, ok := splitNumber(s)
	if !ok {
		return Number{}, Invalid(msgNotANumber)
	}

	digits, point := significant(parts.mantissa)
	if digits == "" {
		return Number{}, nil
	}

	n := Number{Neg: parts.neg, Digits: digits, Exp: parts.exp + point}

	if len(n.Digits) > maxDigits {
		return Number{}, Invalid(msgPrecision)
	}

	switch leading := n.Exp + len(n.Digits) - 1; {
	case leading > maxLeading:
		return Number{}, Invalid(msgOverflow)
	case leading < minLeading:
		return Number{}, Invalid(msgUnderflow)
	default:
		return n, nil
	}
}

// numberText is a number split into its sign, the digits of its mantissa with
// the point kept, and its exponent.
type numberText struct {
	neg      bool
	mantissa string
	exp      int
}

// splitNumber splits s into its parts. ok is false when s is not a number.
func splitNumber(s string) (numberText, bool) {
	var parts numberText

	if s != "" && (s[0] == '+' || s[0] == '-') {
		parts.neg = s[0] == '-'
		s = s[1:]
	}

	mantissa, expText, hasExp := strings.Cut(strings.ReplaceAll(s, "E", "e"), "e")
	if !validMantissa(mantissa) {
		return numberText{}, false
	}

	parts.mantissa = mantissa

	if hasExp {
		exp, ok := parseExponent(expText)
		if !ok {
			return numberText{}, false
		}

		parts.exp = exp
	}

	return parts, true
}

// validMantissa reports whether m is digits with at most one point and at
// least one digit.
func validMantissa(m string) bool {
	digits, points := 0, 0

	for i := range len(m) {
		switch c := m[i]; {
		case c >= '0' && c <= '9':
			digits++
		case c == '.':
			points++
		default:
			return false
		}
	}

	return digits > 0 && points <= 1
}

// parseExponent reads a signed decimal exponent, held to ±expCap.
func parseExponent(s string) (int, bool) {
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}

	if s == "" {
		return 0, false
	}

	exp := 0

	for i := range len(s) {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}

		exp = min(exp*10+int(c-'0'), expCap)
	}

	if neg {
		exp = -exp
	}

	return exp, true
}

// significant returns the digits of mantissa without leading and trailing
// zeros, and the power of ten of the last one.
func significant(mantissa string) (string, int) {
	whole, frac, _ := strings.Cut(mantissa, ".")
	all := strings.TrimLeft(whole+frac, "0")
	trimmed := strings.TrimRight(all, "0")

	return trimmed, len(all) - len(trimmed) - len(frac)
}

// String returns n in DynamoDB's canonical form: plain decimal notation with
// no exponent, no leading or trailing zero, and "0" for zero.
func (n Number) String() string {
	if n.Digits == "" {
		return "0"
	}

	var b strings.Builder

	b.Grow(len(n.Digits) + max(n.Exp, -n.Exp) + signAndPoint)

	if n.Neg {
		b.WriteByte('-')
	}

	switch point := len(n.Digits) + n.Exp; {
	case n.Exp >= 0:
		b.WriteString(n.Digits)
		b.WriteString(strings.Repeat("0", n.Exp))
	case point > 0:
		b.WriteString(n.Digits[:point])
		b.WriteByte('.')
		b.WriteString(n.Digits[point:])
	default:
		b.WriteString("0.")
		b.WriteString(strings.Repeat("0", -point))
		b.WriteString(n.Digits)
	}

	return b.String()
}

// Compare orders numbers by value: -1 when n < o, 0 when equal, 1 when n > o.
func (n Number) Compare(o Number) int {
	if c := n.sign() - o.sign(); c != 0 {
		return clamp(c)
	}

	mag := n.compareMagnitude(o)
	if n.Neg {
		return -mag
	}

	return mag
}

func (n Number) sign() int {
	switch {
	case n.Digits == "":
		return 0
	case n.Neg:
		return -1
	default:
		return 1
	}
}

// compareMagnitude compares the absolute values of two nonzero numbers, or two
// zeros.
func (n Number) compareMagnitude(o Number) int {
	if n.Digits == "" || o.Digits == "" {
		return clamp(len(n.Digits) - len(o.Digits))
	}

	if c := (n.Exp + len(n.Digits)) - (o.Exp + len(o.Digits)); c != 0 {
		return clamp(c)
	}

	// The leading digits stand at the same power, and neither string ends in a
	// zero, so the longer of two strings with a common prefix is the larger.
	return strings.Compare(n.Digits, o.Digits)
}

// size is the bytes DynamoDB counts for the number: one per two significant
// digits, plus one.
func (n Number) size() int {
	return (len(n.Digits)+1)/2 + 1
}

func clamp(c int) int {
	switch {
	case c < 0:
		return -1
	case c > 0:
		return 1
	default:
		return 0
	}
}
