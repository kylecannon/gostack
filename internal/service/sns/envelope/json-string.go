package envelope

import (
	"unicode/utf16"
	"unicode/utf8"
)

const hexDigits = "0123456789abcdef"

// appendString writes s as a JSON string the way Python's json.dumps does
// with ensure_ascii: printable ASCII stays, '"' and '\\' and the five short
// control escapes use their two-character form, and every other rune becomes
// \uXXXX in lowercase hex, a surrogate pair above U+FFFF. An invalid UTF-8
// byte becomes �.
func appendString(b []byte, s string) []byte {
	b = append(b, '"')

	start := 0

	for i := 0; i < len(s); {
		c := s[i]
		if c >= ' ' && c <= '~' && c != '"' && c != '\\' {
			i++

			continue
		}

		b = append(b, s[start:i]...)

		if c < utf8.RuneSelf {
			b = appendASCIIEscape(b, c)
			i++
		} else {
			r, size := utf8.DecodeRuneInString(s[i:])
			b = appendRuneEscape(b, r)
			i += size
		}

		start = i
	}

	b = append(b, s[start:]...)

	return append(b, '"')
}

// AppendJSONDumps writes s as the JSON string Python's json.dumps makes of it,
// for the developer endpoints that return a message as LocalStack does.
func AppendJSONDumps(b []byte, s string) []byte {
	return appendString(b, s)
}

func appendASCIIEscape(b []byte, c byte) []byte {
	switch c {
	case '"', '\\':
		return append(b, '\\', c)
	case '\n':
		return append(b, '\\', 'n')
	case '\r':
		return append(b, '\\', 'r')
	case '\t':
		return append(b, '\\', 't')
	case '\b':
		return append(b, '\\', 'b')
	case '\f':
		return append(b, '\\', 'f')
	default:
		return appendU(b, rune(c))
	}
}

func appendRuneEscape(b []byte, r rune) []byte {
	hi, lo := utf16.EncodeRune(r)
	if bmp := hi == utf8.RuneError; bmp {
		return appendU(b, r)
	}

	return appendU(appendU(b, hi), lo)
}

func appendU(b []byte, r rune) []byte {
	return append(b, '\\', 'u',
		hexDigits[r>>12&0xf], hexDigits[r>>8&0xf], hexDigits[r>>4&0xf], hexDigits[r&0xf])
}
