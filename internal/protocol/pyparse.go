package protocol

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// LocalStack's request parser turns scalars into Python values with int(),
// float() and base64.b64decode(). Those accept more than Go's strconv does, and
// a request that LocalStack takes has to be taken here. The functions below
// follow Python 3.13 and are checked against tables its interpreter produced
// (testdata/python-parse.json).

const (
	maxPyDigits = 4300 // Python refuses int() of a longer digit string
	asciiLimit  = 0x80
	digitChars  = "0123456789"
	// A base64 quad holds four characters. quadTwo is the number of data
	// characters from which a "=" counts as padding.
	quadLen = 4
	quadTwo = 2
	// Bits kept from a data character that is split between two output bytes.
	maskLow4 = 0x0f
	maskLow2 = 0x03
)

// pySpace reports whether r is a space Python's number readers know beyond
// ASCII. They read an ASCII control such as U+001C as text, and strip only the
// six ASCII spaces.
func pySpace(r rune) bool {
	return r >= asciiLimit && unicode.IsSpace(r)
}

// pyDigit returns the value of a Unicode decimal digit.
func pyDigit(r rune) (int, bool) {
	if r >= '0' && r <= '9' {
		return int(r - '0'), true
	}

	if r < asciiLimit || !unicode.IsDigit(r) {
		return 0, false
	}

	start := r
	for unicode.IsDigit(start - 1) {
		start--
	}

	return int(r-start) % decimalBase, true
}

// pyNormalize does what Python does before it reads a number: every Unicode
// space is a space and every Unicode decimal digit is its ASCII digit. It
// reports false for any other character that is not ASCII.
func pyNormalize(s string) (string, bool) {
	ascii := true

	for i := range len(s) {
		if s[i] >= asciiLimit {
			ascii = false

			break
		}
	}

	if ascii {
		return s, true
	}

	var out strings.Builder

	for _, r := range s {
		switch {
		case pySpace(r):
			out.WriteByte(' ')
		case r < asciiLimit:
			out.WriteRune(r)
		default:
			d, ok := pyDigit(r)
			if !ok {
				return "", false
			}

			out.WriteByte(digitChars[d])
		}
	}

	return out.String(), true
}

// pyTrim strips the ASCII spaces Python strips.
func pyTrim(s string) string {
	return strings.Trim(s, " \t\n\v\f\r")
}

// ParsePyInt reads s the way Python's int(s) does: surrounding white space, a
// sign, Unicode digits, and single underscores between digits. A value past the
// int64 range saturates, so callers see a very large number as very large and
// not as malformed. ok is false for what int() refuses.
func ParsePyInt(s string) (v int64, ok bool) {
	s, ok = pyNormalize(s)
	if !ok {
		return 0, false
	}

	s = pyTrim(s)

	neg := false

	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}

	if s == "" || s[0] == '_' || s[len(s)-1] == '_' {
		return 0, false
	}

	var mag uint64

	digits, overflow, prevUnderscore := 0, false, false

	for i := range len(s) {
		c := s[i]

		if c == '_' {
			if prevUnderscore {
				return 0, false
			}

			prevUnderscore = true

			continue
		}

		prevUnderscore = false

		if c < '0' || c > '9' {
			return 0, false
		}

		digits++

		if digits > maxPyDigits {
			return 0, false
		}

		d := uint64(c - '0')
		if mag > (math.MaxUint64-d)/decimalBase {
			overflow = true
		} else {
			mag = mag*decimalBase + d
		}
	}

	return saturate(mag, neg, overflow), true
}

func saturate(mag uint64, neg, overflow bool) int64 {
	switch {
	case neg && (overflow || mag > 1<<63):
		return math.MinInt64
	case neg:
		return -int64(mag) //nolint:gosec // mag <= 1<<63, and -(1<<63) is MinInt64
	case overflow || mag > math.MaxInt64:
		return math.MaxInt64
	default:
		return int64(mag)
	}
}

var pyFloatForm = regexp.MustCompile(
	`^[+-]?(?:\d+(?:_\d+)*(?:\.(?:\d+(?:_\d+)*)?)?|\.\d+(?:_\d+)*)(?:[eE][+-]?\d+(?:_\d+)*)?$`)

// ParsePyFloat reads s the way Python's float(s) does. "inf", "infinity" and
// "nan" are accepted in any case with an optional sign, and a number too large
// for a float64 is an infinity.
func ParsePyFloat(s string) (float64, bool) {
	s, ok := pyNormalize(s)
	if !ok {
		return 0, false
	}

	s = pyTrim(s)

	unsigned := strings.TrimLeft(s, "+-")
	if len(s)-len(unsigned) <= 1 {
		switch strings.ToLower(unsigned) {
		case "inf", "infinity":
			if strings.HasPrefix(s, "-") {
				return math.Inf(-1), true
			}

			return math.Inf(1), true
		case "nan":
			return math.NaN(), true
		}
	}

	if !pyFloatForm.MatchString(s) {
		return 0, false
	}

	f, err := strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}

	return f, true
}

const b64Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

var b64Value = func() (table [256]int8) {
	for i := range table {
		table[i] = -1
	}

	for i := range len(b64Alphabet) {
		table[b64Alphabet[i]] = int8(i)
	}

	return table
}()

// ParsePyBase64 reads s the way Python's base64.b64decode(s) does: characters
// outside the standard alphabet are skipped, a "=" that completes a quad ends
// the data and whatever follows it is ignored, and the data characters have to
// fill whole quads, bar the ones the padding completes. Text that is not ASCII
// is refused.
func ParsePyBase64(s string) ([]byte, bool) {
	out := make([]byte, 0, len(s))
	quad, pads, left := 0, 0, byte(0)

	for i := range len(s) {
		c := s[i]
		if c >= asciiLimit {
			return nil, false
		}

		if c == '=' {
			if quad >= quadTwo {
				pads++

				if quad+pads >= quadLen {
					return out, true
				}
			}

			continue
		}

		v := b64Value[c]
		if v < 0 {
			continue
		}

		pads = 0
		d := byte(v)

		switch quad {
		case 0:
			quad, left = 1, d
		case 1:
			quad = 2
			out = append(out, left<<2|d>>4)
			left = d & maskLow4
		case quadTwo:
			quad = 3
			out = append(out, left<<4|d>>2)
			left = d & maskLow2
		default:
			quad = 0
			out = append(out, left<<6|d)
		}
	}

	if quad != 0 {
		return nil, false
	}

	return out, true
}
