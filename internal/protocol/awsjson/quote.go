package awsjson

import (
	"encoding/binary"
	"encoding/json/jsontext"
	"math/bits"
	"slices"
	"unicode/utf8"
)

const (
	wordBytes = 8
	byteBits  = 8
	ones      = 0x0101010101010101
	highs     = 0x8080808080808080

	// escapeLenShift puts an escape's length in the top byte of its word.
	escapeLenShift = 56
)

// AppendQuote appends s to dst as a json string, byte for byte as v2 writes it
// when it allows invalid UTF-8: each byte of s that is not part of a valid
// sequence is written as U+FFFD, and no error is reported. It is the quoter for
// output that must not fail on a string a client sent, such as a snapshot file;
// the response encoders refuse invalid UTF-8 and use appendStrictQuote.
func AppendQuote(dst []byte, s string) []byte {
	out, ok := appendStrictQuote(dst, s)
	if ok {
		return out
	}

	// appendStrictQuote stopped half way and may have grown dst: cut what it wrote
	// from the slice it returned, and let jsontext write the string with the
	// replacement characters. Its error says only that it replaced.
	out, _ = jsontext.AppendQuote(out[:len(dst)], s)

	return out
}

// appendStrictQuote appends s to dst as a json string, byte for byte as v2 writes
// it with the options of this package: '"', '\\' and the bytes below 0x20 are
// escaped, \b \f \n \r \t by name and the rest as \u00xx; '<', '>', '&',
// U+2028 and U+2029 are not. It reports false when s is not valid UTF-8, which
// v2 refuses.
//
// v2 quotes a byte per loop turn, and that loop keeps three values on the
// stack, so its cost moves with the stack pointer's place in a cache line: on
// AMD Zen 3 and 4 a 16-byte change in a caller's frame moved a 1 MiB
// ReceiveMessage by 38% (SQ-455). Here plain text, ASCII with nothing to
// escape, is skipped a word at a time and copied in runs. From the first byte
// that is not plain, a loop shaped like v2's takes a byte at a time: it checks
// each multi-byte sequence in line, escapes each byte that needs it, and goes
// back to the words after longRun plain bytes in a row, so text that alternates
// often between ASCII and other scripts stays in it. Its loops call nothing, so
// they keep their values in registers.
func appendStrictQuote(dst []byte, s string) ([]byte, bool) {
	dst = slices.Grow(dst, len(s)+len(`""`))
	dst = append(dst, '"')

	// s[start:n] is quoted as it is and not yet copied.
	start, n := 0, 0

	for n < len(s) {
		// The lowest bit specials sets is the first byte that is not plain.
		for len(s)-n >= wordBytes {
			if m := specials(load64(s[n:])); m != 0 {
				n += bits.TrailingZeros64(m) / byteBits

				break
			}

			n += wordBytes
		}

		for {
			// A run of longRun plain bytes goes back to the words.
			end := min(len(s), n+longRun)
			for n < end && plainBytes[s[n]] {
				n++
			}

			if n == end {
				break
			}

			// decodeAt, for the sequences sequence refuses, takes n and gives
			// it back, so that n is not kept on the stack across the call.
			c, t, size := s[n], s[n:], 0
			if c >= utf8.RuneSelf && len(t) >= utf8.UTFMax {
				size = sequence(c, t[1], t[2], t[3])
			}

			switch {
			case size != 0:
				n += size
			case c < utf8.RuneSelf:
				dst = append(dst, s[start:n]...)
				if cap(dst)-len(dst) < wordBytes {
					dst = slices.Grow(dst, wordBytes)
				}

				e, l := escapes[c], len(dst)
				binary.LittleEndian.PutUint64(dst[l:l+wordBytes], e)
				dst = dst[:l+int(e>>escapeLenShift)]
				n++
				start = n
			default:
				if n = decodeAt(s, n); n < 0 {
					return dst, false
				}
			}
		}
	}

	dst = append(dst, s[start:]...)

	return append(dst, '"'), true
}

// decodeAt returns the index after the UTF-8 sequence at s[n], or -1 when it
// is not valid.
//
//go:noinline
func decodeAt(s string, n int) int {
	if _, size := utf8.DecodeRuneInString(s[n:]); size > 1 {
		return n + size
	}

	return -1
}

// longRun is how many plain bytes in a row appendStrictQuote's byte loop takes
// before it goes back to testing words.
const longRun = 2 * wordBytes

// sequence returns the length of the UTF-8 sequence that starts with c and
// the bytes b1, b2 and b3 after it, or 0 when they start no valid sequence.
// It rules out overlong forms, surrogates and code points past U+10FFFF as
// unicode/utf8 does. It needs no call, so the byte loop's values stay in
// registers through text in other scripts; utf8.DecodeRuneInString settles
// what it refuses, and the last bytes of s, which it cannot read four of.
func sequence(c, b1, b2, b3 byte) int {
	const three = 3

	l := leads[c]
	if b1-l.lo > l.span || l.size >= three && !continues(b2) || l.size > three && !continues(b3) {
		return 0
	}

	return int(l.size)
}

// continues reports whether b is a continuation byte, 0b10xxxxxx.
func continues(b byte) bool {
	const mask, cont = 0xc0, 0x80

	return b&mask == cont
}

// lead is what a byte of 0x80 or more starts: a UTF-8 sequence of size bytes
// whose second byte is at least lo and at most lo+span, or, with a size of 0,
// none. The pad makes an entry four bytes, which an index reaches by a shift.
type lead struct{ lo, span, size, _ byte }

var leads = func() (t [256]lead) {
	// The first bytes of two-, three- and four-byte sequences, the range of a
	// continuation byte, and the narrower second-byte ranges that unicode/utf8
	// gives four first bytes, which rule out overlong forms, surrogates and
	// code points past U+10FFFF.
	const (
		first2, first3, first4, firstMax = 0xc2, 0xe0, 0xf0, 0xf4
		contLo, contHi                   = 0x80, 0xbf
		e0Lo, edHi, f0Lo, f4Hi           = 0xa0, 0x9f, 0x90, 0x8f
		surrogates                       = 0xed
		two, three, four                 = 2, 3, 4
	)

	for c := first2; c <= firstMax; c++ {
		lo, hi, size := byte(contLo), byte(contHi), byte(two)

		switch {
		case c >= first4:
			size = four
		case c >= first3:
			size = three
		default:
		}

		switch c {
		case first3:
			lo = e0Lo
		case surrogates:
			hi = edHi
		case first4:
			lo = f0Lo
		case firstMax:
			hi = f4Hi
		default:
		}

		t[c] = lead{lo: lo, span: hi - lo, size: size}
	}

	return t
}()

// plainBytes is true for the bytes appendStrictQuote writes as they are on their
// own: ASCII that json does not escape.
var plainBytes = func() (t [256]bool) {
	for c := range utf8.RuneSelf {
		t[c] = c >= ' ' && c != '"' && c != '\\'
	}

	return t
}()

// specials returns x with the high bit set in each byte that json escapes or
// that is 0x80 or more, and no bit set below the lowest such byte. A borrow
// can also set the bits of the bytes above a match, never below one, so it
// is zero exactly when no byte is special.
func specials(x uint64) uint64 {
	quote := x ^ (ones * '"')
	slash := x ^ (ones * '\\')
	m := x | (x-ones*' ')&^x
	m |= (quote - ones) &^ quote
	m |= (slash - ones) &^ slash

	return m & highs
}

// load64 reads the first eight bytes of s little-endian, which the compiler
// makes one load.
func load64(s string) uint64 {
	_ = s[7]

	return uint64(s[0]) | uint64(s[1])<<8 | uint64(s[2])<<16 | uint64(s[3])<<24 |
		uint64(s[4])<<32 | uint64(s[5])<<40 | uint64(s[6])<<48 | uint64(s[7])<<56
}

// escapes holds, for each ASCII byte appendStrictQuote escapes, its json escape in
// the low bytes of a word in the order they are written, with its length in
// the top byte; it is zero for the bytes written as they are.
var escapes = func() (t [utf8.RuneSelf]uint64) {
	const hex = "0123456789abcdef"

	word := func(e string) uint64 {
		var b [wordBytes]byte
		copy(b[:], e)

		return binary.LittleEndian.Uint64(b[:]) | uint64(len(e))<<escapeLenShift
	}

	for c := range byte(' ') {
		t[c] = word(`\u00` + string(hex[c>>4]) + string(hex[c&0xf]))
	}

	t['\b'], t['\f'], t['\n'], t['\r'], t['\t'] = word(`\b`), word(`\f`), word(`\n`), word(`\r`), word(`\t`)
	t['"'], t['\\'] = word(`\"`), word(`\\`)

	return t
}()
