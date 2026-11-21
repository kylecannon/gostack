package awsjson

import (
	"encoding/binary"
	"math/bits"
	"slices"
	"unicode/utf8"
)

const (
	wordBytes = 8
	byteBits  = 8
	ones      = 0x0101010101010101
	highs     = 0x8080808080808080

	// twoByteLow and twoByteHigh bound the first byte of a valid two-byte
	// UTF-8 sequence, and a continuation byte is 0b10xxxxxx.
	twoByteLow       = 0xc2
	twoByteHigh      = 0xdf
	continuationMask = 0xc0
	continuation     = 0x80

	// escapeLenShift puts an escape's length in the top byte of its word.
	escapeLenShift = 56
)

// appendQuote appends s to dst as a json string, byte for byte as v2 writes
// it with the options of this package: '"', '\\' and the bytes below 0x20 are
// escaped, \b \f \n \r \t by name and the rest as \u00xx; '<', '>', '&',
// U+2028 and U+2029 are not. It reports false when s is not valid UTF-8, which
// v2 refuses.
//
// v2 quotes a byte per loop turn, and that loop keeps three values on the
// stack, so its cost moves with the stack pointer's place in a cache line: on
// AMD Zen 3 and 4 a 16-byte change in a caller's frame moved a 1 MiB
// ReceiveMessage by 38% (SQ-455). This tests eight bytes per turn for one to
// escape or to check as UTF-8, and copies each clean run whole. Text that is
// not ASCII is checked a stretch at a time, escapes next to each other are
// written without going back to the word test, and a short run or an escape
// is written with one eight-byte store, so text dense with either costs no
// more than v2's loop.
func appendQuote(dst []byte, s string) ([]byte, bool) {
	// The text and its quotes, and a word of room for the stores.
	dst = slices.Grow(dst, len(s)+len(`""`)+wordBytes)
	dst = append(dst, '"')
	start, i := 0, 0

	for {
		var x uint64
		if len(s)-i >= wordBytes {
			x = load64(s[i:])
		} else {
			x = loadTail(s[i:])
		}

		m := specials(x)
		if m == 0 {
			i += wordBytes

			continue
		}

		// Past the end, the word holds zero bytes, which specials marks.
		if i += bits.TrailingZeros64(m) / byteBits; i >= len(s) {
			break
		}

		c := s[i]
		if c >= utf8.RuneSelf {
			// A lone two-byte character, the common case in Latin text, is
			// checked in place.
			if len(s)-i > 2 && c-twoByteLow <= twoByteHigh-twoByteLow &&
				s[i+1]&continuationMask == continuation && s[i+2] < utf8.RuneSelf {
				i += 2

				continue
			}

			n := multiByteRun(s[i:])
			if n == 0 {
				return dst, false
			}

			i += n

			continue
		}

		dst, i = appendEscapes(dst, s, start, i)
		start = i
	}

	dst = append(dst, s[start:]...)

	return append(dst, '"'), true
}

// specials returns x with the high bit set in each byte that json escapes or
// that is 0x80 or more, and no bit set below the lowest such byte. A borrow
// can also set the bits of the bytes above a match, never below one, so the
// lowest set bit is exact.
func specials(x uint64) uint64 {
	quote := x ^ (ones * '"')
	slash := x ^ (ones * '\\')
	m := x | (x-ones*' ')&^x
	m |= (quote - ones) &^ quote
	m |= (slash - ones) &^ slash

	return m & highs
}

// appendEscapes appends s[start:i], a run with nothing to escape, then the
// escape of s[i] and of each byte right after it that json escapes. It returns
// the index after the last. A run of a word or less is copied with one
// eight-byte store, and so is each escape, which saves a copy call for each
// where escapes come close together.
func appendEscapes(dst []byte, s string, start, i int) ([]byte, int) {
	if l := len(dst); i-start <= wordBytes && len(s)-start >= wordBytes && cap(dst)-l >= wordBytes {
		binary.LittleEndian.PutUint64(dst[l:l+wordBytes], load64(s[start:]))
		dst = dst[:l+i-start]
	} else {
		dst = append(dst, s[start:i]...)
	}

	for ; i < len(s) && isEscaped(s[i]); i++ {
		if cap(dst)-len(dst) < wordBytes {
			dst = slices.Grow(dst, wordBytes)
		}

		e, l := escapes[s[i]], len(dst)
		binary.LittleEndian.PutUint64(dst[l:l+wordBytes], e)
		dst = dst[:l+int(e>>escapeLenShift)]
	}

	return dst, i
}

// isEscaped reports whether json escapes the ASCII byte c.
func isEscaped(c byte) bool {
	return c < ' ' || c == '"' || c == '\\'
}

// multiByteRun returns the length of the run of bytes of 0x80 and above that
// starts s, or 0 when that run is not valid UTF-8. A multi-byte sequence holds
// no ASCII byte, so a string is valid UTF-8 exactly when each such run is.
func multiByteRun(s string) int {
	n := 0
	for ; len(s)-n >= wordBytes; n += wordBytes {
		if m := ^load64(s[n:]) & highs; m != 0 {
			n += bits.TrailingZeros64(m) / byteBits

			break
		}
	}

	for n < len(s) && s[n] >= utf8.RuneSelf {
		n++
	}

	if !utf8.ValidString(s[:n]) {
		return 0
	}

	return n
}

// loadTail reads s, shorter than a word, followed by zero bytes.
func loadTail(s string) uint64 {
	var b [wordBytes]byte
	copy(b[:], s)

	return binary.LittleEndian.Uint64(b[:])
}

// load64 reads the first eight bytes of s little-endian, which the compiler
// makes one load.
func load64(s string) uint64 {
	_ = s[7]

	return uint64(s[0]) | uint64(s[1])<<8 | uint64(s[2])<<16 | uint64(s[3])<<24 |
		uint64(s[4])<<32 | uint64(s[5])<<40 | uint64(s[6])<<48 | uint64(s[7])<<56
}

// escapes holds the json escape of each ASCII byte appendQuote escapes, in the
// low bytes of a word in the order they are written, with its length in the
// top byte.
var escapes = func() (t ['\\' + 1]uint64) {
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
