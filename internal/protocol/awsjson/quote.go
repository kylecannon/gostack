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
	lows      = 0x7f7f7f7f7f7f7f7f

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
// ReceiveMessage by 38% (SQ-455). Here scan does the work a word at a time
// and calls nothing, so that it keeps its values in registers; it stops for
// the few things that need a call, which appendQuote does before it goes on.
func appendQuote(dst []byte, s string) ([]byte, bool) {
	dst = slices.Grow(dst, len(s)+len(`""`)+wordReserve)
	dst = append(dst, '"')

	var cur cursor

	for {
		var stop int

		dst, cur, stop = scan(dst, s, cur)

		switch stop {
		case stopRoom:
			dst = slices.Grow(dst, len(s)-cur.start+wordReserve)
		case stopRun:
			// at is a byte to escape in the word at w, after a long run or
			// at the end of s. The continuation bytes from at on are
			// counted again when scan goes on from there.
			cur.balance -= balanceOf(wordFrom(s, cur.w, cur.at))
			dst = append(dst, s[cur.start:cur.at]...)
			dst = appendEscape(dst, s[cur.at])
			cur.w, cur.start = cur.at+1, cur.at+1
		case stopFirst:
			// at starts an invalid sequence, or one that s ends within four
			// bytes of. scan counted what at calls for, and counts the
			// continuation bytes after it again when it goes on from there.
			if _, n := utf8.DecodeRuneInString(s[cur.at:]); n == 1 {
				return dst, false
			}

			cur.balance -= balanceOf(wordFrom(s, cur.w, cur.at+1))
			cur.w = cur.at + 1
		case stopHigh:
			// The word at w holds nothing but bytes of 0x80 or more, so it is
			// most likely inside a stretch of text in another script, which
			// is checked whole from its first sequence that starts in the
			// word. The continuation bytes before that one belong to the
			// sequence before.
			x := load64(s[cur.w:])

			first := x & (x << 1) & highs
			if first == 0 {
				return dst, false
			}

			j := cur.w + bits.TrailingZeros64(first)/byteBits
			k := j + highRun(s[j:])

			if !utf8.ValidString(s[j:k]) {
				return dst, false
			}

			cur.balance += j - cur.w
			cur.w = k
		case stopEnd:
			// Each first byte checked that its continuation bytes follow it,
			// so the text is valid when no other continuation byte is left.
			if cur.balance != 0 {
				return dst, false
			}

			dst = append(dst, s[cur.start:]...)

			return append(dst, '"'), true
		default:
			return dst, false
		}
	}
}

// cursor is where appendQuote is in s: w is the word scan tests next, at the
// byte it stopped at, start where the run not yet copied begins, and balance
// counts the continuation bytes seen less those that follow a valid first
// byte. scan keeps it in four registers.
type cursor struct{ w, at, start, balance int }

// What scan stopped for.
const (
	stopEnd   = iota // s is done.
	stopFirst        // a first byte scan did not find valid.
	stopRoom         // dst needs room for the rest of s.
	stopRun          // a byte to escape follows a run too long to copy here.
	stopHigh         // a word of bytes of 0x80 or more.
)

// maxRun is the longest run before an escape that scan copies itself, with
// four eight-byte stores; past it, appendQuote's copy call costs less.
const maxRun = 4 * wordBytes

// wordReserve is the room in dst, past what the rest of s takes as it is,
// that scan needs to write the escapes of one word: each adds at most five
// bytes, and each run and escape is written with eight-byte stores that may
// reach seven bytes past it.
const wordReserve = 2 * wordBytes * wordBytes

// scan appends s from cur on to dst, as appendQuote does, until it is done or
// must stop for a call, and returns where it stopped and what for. It tests
// eight bytes per turn and copies each clean run whole.
// In a word that holds a byte to escape or a first byte of a UTF-8 sequence,
// it visits only those: an escape is written with one eight-byte store, and
// so is a short run before it, and a sequence is checked from one four-byte
// load and a table entry, so text dense with either costs a few operations
// per such byte, where v2 spends a loop turn on every byte.
func scan(dst []byte, s string, cur cursor) ([]byte, cursor, int) {
	w, start, balance := cur.w, cur.start, cur.balance

	for {
		if len(s)-w >= wordBytes && specials(load64(s[w:])) == 0 {
			w = cleanWords(s, w+wordBytes)
		}

		if w >= len(s) {
			return dst, cursor{w, w, start, balance}, stopEnd
		}

		if cap(dst)-len(dst) < len(s)-start+wordReserve {
			return dst, cursor{w, w, start, balance}, stopRoom
		}

		var x uint64
		if len(s)-w >= wordBytes {
			if x = load64(s[w:]); x&highs == highs {
				return dst, cursor{w, w, start, balance}, stopHigh
			}
		} else {
			x = loadTail(s[w:])
		}

		balance += balanceOf(x)

		// The zero bytes that loadTail adds past the end are not marked.
		for m := marks(x) & firstBytes(len(s)-w); m != 0; m &= m - 1 {
			j := w + bits.TrailingZeros64(m)/byteBits

			c := s[j]
			if c >= utf8.RuneSelf {
				// appendQuote checks a sequence that s may end within.
				if len(s)-j < runeMax || badFirst(load32(s[j:]), c) {
					return dst, cursor{w, j, start, balance}, stopFirst
				}

				continue
			}

			// A short run is copied with one store or four, which read and
			// write up to a word or four from its start: s holds that many
			// bytes unless the run is near its end, and dst has the room scan
			// keeps. appendQuote copies a longer run, or one near the end.
			l, n := len(dst), j-start
			switch {
			case n <= wordBytes && len(s)-start >= wordBytes:
				binary.LittleEndian.PutUint64(dst[l:l+wordBytes], load64(s[start:]))
			case n <= maxRun && len(s)-start >= maxRun:
				binary.LittleEndian.PutUint64(dst[l:l+wordBytes], load64(s[start:]))
				binary.LittleEndian.PutUint64(dst[l+wordBytes:l+2*wordBytes], load64(s[start+wordBytes:]))
				binary.LittleEndian.PutUint64(dst[l+2*wordBytes:l+3*wordBytes], load64(s[start+2*wordBytes:]))
				binary.LittleEndian.PutUint64(dst[l+3*wordBytes:l+maxRun], load64(s[start+3*wordBytes:]))
			default:
				return dst, cursor{w, j, start, balance}, stopRun
			}

			e, l := escapes[c], l+n
			binary.LittleEndian.PutUint64(dst[l:l+wordBytes], e)
			dst = dst[:l+int(e>>escapeLenShift)]
			start = j + 1
		}

		w += wordBytes
	}
}

// firstBytes returns a mask of the first n bytes of a word, or of all of them
// when n is a word or more.
func firstBytes(n int) uint64 {
	if n >= wordBytes {
		return ^uint64(0)
	}

	return 1<<(n*byteBits) - 1
}

// cleanWords returns the index of the first word from w on that holds a byte
// specials marks, or of the part shorter than a word at the end. It is its
// own function so that the loop over plain text keeps its few values in
// registers, whatever scan around it has to keep on the stack.
//
//go:noinline
func cleanWords(s string, w int) int {
	for ; len(s)-w >= wordBytes; w += wordBytes {
		if specials(load64(s[w:])) != 0 {
			break
		}
	}

	return w
}

// badFirst reports whether c, a byte of 0x80 or more, starts no valid UTF-8
// sequence, from v, the four bytes from c on read little-endian.
func badFirst(v uint32, c byte) bool {
	l := &leads[c]

	return byte(v>>byteBits)-l.lo > l.span || v&l.mask != l.want //nolint:gosec // takes the second byte of v on purpose
}

// wordFrom returns the word of s at w with the bytes before j set to zero.
func wordFrom(s string, w, j int) uint64 {
	var x uint64
	if len(s)-w >= wordBytes {
		x = load64(s[w:])
	} else {
		x = loadTail(s[w:])
	}

	shift := (j - w) * byteBits

	return x >> shift << shift
}

// appendEscape appends the escape of the ASCII byte c.
func appendEscape(dst []byte, c byte) []byte {
	e := escapes[c]
	var b [wordBytes]byte
	binary.LittleEndian.PutUint64(b[:], e)

	return append(dst, b[:e>>escapeLenShift]...)
}

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

// marks returns x with the high bit set in each byte that json escapes and in
// each of 0xc0 or more, which can start a UTF-8 sequence. Seven bits of each
// byte are added to without a carry out of the byte, so no match sets a bit in
// another.
func marks(x uint64) uint64 {
	quote := x ^ (ones * '"')
	slash := x ^ (ones * '\\')
	below := (x&lows + ones*(utf8.RuneSelf-' ')) | x
	quote |= quote&lows + lows
	slash |= slash&lows + lows

	return (x&(x<<1) | ^below | ^quote | ^slash) & highs
}

// balanceOf returns the number of continuation bytes in x, 0b10xxxxxx, less
// the number that the bytes in x that can start a sequence call for: one
// after 0b110xxxxx, two after 0b1110xxxx and three after 0b11110xxx. Each
// first byte that scan finds valid has its continuation bytes after it, so s
// is valid UTF-8 when the sum over its words is zero.
func balanceOf(x uint64) int {
	// A first byte's second, third and fourth bits, shifted to its top.
	const second, third, fourth = 1, 2, 3

	high := x & highs
	first := high & (x << second)
	three := first & (x << third)
	four := three & (x << fourth)

	const low = byteBits - 1

	return sumBytes((high&^first)>>low) - sumBytes(first>>low+three>>low+four>>low)
}

// highRun returns the length of the run of bytes of 0x80 and above that
// starts s.
func highRun(s string) int {
	n := 0
	for ; len(s)-n >= wordBytes; n += wordBytes {
		if m := ^load64(s[n:]) & highs; m != 0 {
			return n + bits.TrailingZeros64(m)/byteBits
		}
	}

	for n < len(s) && s[n] >= utf8.RuneSelf {
		n++
	}

	return n
}

// runeMax is the length of the longest UTF-8 sequence.
const runeMax = utf8.UTFMax

// lead is how a byte of 0x80 or more starts a UTF-8 sequence, read
// little-endian from that byte: the second byte is at least lo and at most
// lo+span, which also rules out overlong forms, surrogates and code points
// past U+10FFFF as unicode/utf8 does, and the bytes after it that mask selects
// are want, continuation bytes.
// A byte that starts no sequence has a want no value matches.
type lead struct {
	mask, want uint32
	lo, span   byte
}

var leads = func() (t [256]lead) {
	// The first bytes of two-, three- and four-byte sequences, the range
	// of a continuation byte, and the narrower second-byte ranges that
	// unicode/utf8 gives four first bytes.
	const (
		first2, first3, first4, firstMax = 0xc2, 0xe0, 0xf0, 0xf4
		contLo, contHi                   = 0x80, 0xbf
		e0Lo, edHi, f0Lo, f4Hi           = 0xa0, 0x9f, 0x90, 0x8f
		surrogates                       = 0xed

		// The top two bits of the third and fourth bytes, and what a
		// continuation byte has there.
		third, thirdWant          = 0x00c0_0000, 0x0080_0000
		fourth, fourthWant        = 0xc0c0_0000, 0x8080_0000
		noSequence         uint32 = 1
	)

	for c := utf8.RuneSelf; c < len(t); c++ {
		lo, hi := byte(contLo), byte(contHi)

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

		switch {
		case c < first2 || c > firstMax:
			t[c] = lead{want: noSequence}
		case c < first3:
			t[c] = lead{lo: lo, span: hi - lo}
		case c < first4:
			t[c] = lead{mask: third, want: thirdWant, lo: lo, span: hi - lo}
		default:
			t[c] = lead{mask: fourth, want: fourthWant, lo: lo, span: hi - lo}
		}
	}

	return t
}()

// sumBytes returns the sum of the bytes of x, when it is below 256: the
// multiplication adds each byte into the top one.
func sumBytes(x uint64) int {
	return int(x * ones >> escapeLenShift)
}

// loadTail reads s, shorter than a word, followed by zero bytes.
func loadTail(s string) uint64 {
	var x uint64
	for i := len(s) - 1; i >= 0; i-- {
		x = x<<byteBits | uint64(s[i])
	}

	return x
}

// load64 reads the first eight bytes of s little-endian, which the compiler
// makes one load.
func load64(s string) uint64 {
	_ = s[7]

	return uint64(s[0]) | uint64(s[1])<<8 | uint64(s[2])<<16 | uint64(s[3])<<24 |
		uint64(s[4])<<32 | uint64(s[5])<<40 | uint64(s[6])<<48 | uint64(s[7])<<56
}

// load32 reads the first four bytes of s little-endian as one load.
func load32(s string) uint32 {
	_ = s[3]

	return uint32(s[0]) | uint32(s[1])<<8 | uint32(s[2])<<16 | uint32(s[3])<<24
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
