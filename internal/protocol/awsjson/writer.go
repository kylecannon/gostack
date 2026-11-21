package awsjson

import (
	"encoding/base64"
	"errors"
	"math"
	"math/bits"
	"strconv"
	"unicode/utf8"

	"github.com/kylecannon/gostack/internal/protocol"
)

// Appender is a value with a generated json encoder. Every output type of a
// json service is one: AppendJSON appends the value to b, byte for byte as
// encoding/json/v2 writes it with the options of this package, without
// reflection. The error is the first value v2 refuses, invalid UTF-8 in a
// string; Encode then runs v2 itself, so the error is v2's own.
type Appender interface {
	AppendJSON(b []byte) ([]byte, error)
}

// errInvalidUTF8 is a string the generated encoder cannot write; v2 refuses
// it too.
var errInvalidUTF8 = errors.New("string is not valid UTF-8")

// Writer appends json to a buffer for the generated encoders. Its methods
// write what v2 writes for the same Go value. After an error it keeps
// appending, and Result reports the first error.
type Writer struct {
	buf []byte
	err error
}

// NewWriter returns a Writer that appends to buf.
func NewWriter(buf []byte) Writer {
	return Writer{buf: buf}
}

// Result returns the buffer and the first error.
func (w *Writer) Result() ([]byte, error) {
	return w.buf, w.err
}

// Raw appends json text the generator wrote: a member name with its
// punctuation.
func (w *Writer) Raw(s string) {
	w.buf = append(w.buf, s...)
}

// Byte appends one byte of punctuation.
func (w *Writer) Byte(c byte) {
	w.buf = append(w.buf, c)
}

// Null appends null: a nil pointer, slice or map inside a list or a map.
func (w *Writer) Null() {
	w.buf = append(w.buf, "null"...)
}

// ObjectStart marks where an object's members begin. Each member is written
// with a leading comma; ObjectEnd turns the first one into the opening brace.
func (w *Writer) ObjectStart() int {
	return len(w.buf)
}

// ObjectEnd closes the object begun at start.
func (w *Writer) ObjectEnd(start int) {
	if len(w.buf) == start {
		w.buf = append(w.buf, '{', '}')

		return
	}

	w.buf[start] = '{'
	w.buf = append(w.buf, '}')
}

// String appends s quoted.
func (w *Writer) String(s string) {
	var ok bool

	w.buf, ok = appendQuote(w.buf, s)
	if !ok && w.err == nil {
		w.err = errInvalidUTF8
	}
}

// Int32 appends v.
func (w *Writer) Int32(v int32) {
	w.buf = strconv.AppendInt(w.buf, int64(v), decimalBase)
}

// Int64 appends v.
func (w *Writer) Int64(v int64) {
	w.buf = strconv.AppendInt(w.buf, v, decimalBase)
}

// Bool appends v.
func (w *Writer) Bool(v bool) {
	w.buf = strconv.AppendBool(w.buf, v)
}

// Float64 appends v as v2 does, and NaN and the infinities as the strings
// marshalNonFinite writes on the lenient pass.
func (w *Writer) Float64(v float64) {
	switch {
	case math.IsNaN(v):
		w.buf = append(w.buf, `"NaN"`...)
	case math.IsInf(v, 1):
		w.buf = append(w.buf, `"Infinity"`...)
	case math.IsInf(v, -1):
		w.buf = append(w.buf, `"-Infinity"`...)
	default:
		w.buf = appendFloat(w.buf, v)
	}
}

// appendFloat is encoding/json/internal/jsonwire.AppendFloat for 64 bits: the
// shortest decimal, in exponent form below 1e-6 and from 1e21 up, with a
// one-digit negative exponent written without its leading zero.
func appendFloat(dst []byte, f float64) []byte {
	format := byte('f')
	if abs := math.Abs(f); abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}

	dst = strconv.AppendFloat(dst, f, format, -1, float64Bits)

	if n := len(dst); format == 'e' && n >= 4 && dst[n-4] == 'e' && dst[n-3] == '-' && dst[n-2] == '0' {
		dst[n-2] = dst[n-1]
		dst = dst[:n-1]
	}

	return dst
}

// Blob appends v in standard base64 with padding, and nil as null.
func (w *Writer) Blob(v []byte) {
	if v == nil {
		w.Null()

		return
	}

	w.buf = append(w.buf, '"')
	w.buf = base64.StdEncoding.AppendEncode(w.buf, v)
	w.buf = append(w.buf, '"')
}

// Time appends t as epoch seconds, and nil as null.
func (w *Writer) Time(t *protocol.Time) {
	if t == nil {
		w.Null()

		return
	}

	w.buf = t.AppendEpoch(w.buf)
}

// Reflect appends v as v2 writes it by reflection, for the shapes the
// generator leaves to v2.
func (w *Writer) Reflect(v any) {
	b, err := marshalStrictFirst(v)
	if err != nil {
		if w.err == nil {
			w.err = err
		}

		return
	}

	w.buf = append(w.buf, b...)
}

// appendQuote appends s to dst as a json string, byte for byte as v2 writes
// it with the options of this package: '"', '\\' and the bytes below 0x20 are
// escaped, \b \f \n \r \t by name and the rest as \u00xx; '<', '>', '&',
// U+2028 and U+2029 are not. It reports false when s is not valid UTF-8, which
// v2 refuses.
//
// v2 quotes a byte per loop turn, and that loop keeps three values on the
// stack, so its cost moves with the stack pointer's place in a cache line: on
// AMD Zen 3 and 4 a 16-byte change in a caller's frame moved a 1 MiB
// ReceiveMessage by 38% (SQ-455). This scans eight bytes per turn for the next
// byte to escape or to check as UTF-8, and copies each clean run whole.
func appendQuote(dst []byte, s string) ([]byte, bool) {
	dst = append(dst, '"')
	start, i := 0, 0

	for {
		i += nextSpecial(s[i:])
		if i == len(s) {
			break
		}

		if c := s[i]; c >= utf8.RuneSelf {
			r, n := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && n == 1 {
				return dst, false
			}

			i += n

			continue
		}

		dst = append(dst, s[start:i]...)
		dst = appendEscaped(dst, s[i])
		i++
		start = i
	}

	dst = append(dst, s[start:]...)

	return append(dst, '"'), true
}

// decimalBase and float64Bits are the base of an integer and the width of a
// float as json writes them.
const (
	decimalBase = 10
	float64Bits = 64
)

const (
	ones  = 0x0101010101010101
	highs = 0x8080808080808080
)

// nextSpecial returns the index of the first byte of s that json escapes or
// that starts a multi-byte UTF-8 sequence, or len(s).
func nextSpecial(s string) int {
	i := 0

	for ; len(s)-i >= 8; i += 8 {
		x := load64(s[i : i+8])

		// A byte of 0x80 or more, one below 0x20, or one equal to '"' or '\\'
		// sets the high bit of its own byte. A borrow can also set the bits
		// of the bytes above a match, never below one, so the lowest set bit
		// is exact.
		quote := x ^ (ones * '"')
		slash := x ^ (ones * '\\')
		m := x | (x-ones*0x20)&^x
		m |= (quote - ones) &^ quote
		m |= (slash - ones) &^ slash

		if m &= highs; m != 0 {
			return i + bits.TrailingZeros64(m)/8
		}
	}

	for ; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == '"' || c == '\\' || c >= utf8.RuneSelf {
			return i
		}
	}

	return i
}

// load64 reads the eight bytes of s little-endian, which the compiler makes one
// load.
func load64(s string) uint64 {
	_ = s[7]

	return uint64(s[0]) | uint64(s[1])<<8 | uint64(s[2])<<16 | uint64(s[3])<<24 |
		uint64(s[4])<<32 | uint64(s[5])<<40 | uint64(s[6])<<48 | uint64(s[7])<<56
}

// appendEscaped appends the json escape of c, an ASCII byte nextSpecial
// stopped at.
func appendEscaped(dst []byte, c byte) []byte {
	const hex = "0123456789abcdef"

	switch c {
	case '"', '\\':
		return append(dst, '\\', c)
	case '\b':
		return append(dst, '\\', 'b')
	case '\f':
		return append(dst, '\\', 'f')
	case '\n':
		return append(dst, '\\', 'n')
	case '\r':
		return append(dst, '\\', 'r')
	case '\t':
		return append(dst, '\\', 't')
	default:
		return append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
	}
}
