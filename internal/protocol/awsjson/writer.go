package awsjson

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"

	"github.com/kylecannon/gostack/internal/protocol"
)

// Appender is a value with a generated json encoder. Every output type of a
// json service is one: AppendJSON appends the value to b, byte for byte as
// encoding/json/v2 writes it with the options of this package, without
// reflection. The error is the first value v2 refuses, invalid UTF-8 in a
// string; Encode then runs v2 itself, so the error is v2's own, unless the
// value holds a member written from its encoded sibling (Result). The encoder
// does not count depth: v2 refuses a value nested past 10000 levels, and the
// encoder writes it. No output nests that deep: DynamoDB refuses an attribute
// value past 32 levels.
type Appender interface {
	AppendJSON(b []byte) ([]byte, error)
}

// errInvalidUTF8 is a string the generated encoder cannot write; v2 refuses
// it too.
var errInvalidUTF8 = errors.New("string is not valid UTF-8")

// errEncodedRefused wraps the error of a value that also holds a member
// written from its encoded sibling. v2 never reads a sibling and would write
// the value without that member, so Encode and WriteResponse answer the error
// instead of running v2.
var errEncodedRefused = errors.New("a response with pre-encoded members holds a value json cannot write")

// Writer appends json to a buffer for the generated encoders. Its methods
// write what v2 writes for the same Go value. After an error it keeps
// appending, and Result reports the first error.
type Writer struct {
	buf []byte
	err error
	// encoded is set once a member is written from its encoded sibling.
	encoded bool
}

// NewWriter returns a Writer that appends to buf.
func NewWriter(buf []byte) Writer {
	return Writer{buf: buf}
}

// Result returns the buffer and the first error, wrapped in errEncodedRefused
// when a member was written from its encoded sibling.
func (w *Writer) Result() ([]byte, error) {
	if w.err != nil && w.encoded {
		return w.buf, fmt.Errorf("%w: %w", errEncodedRefused, w.err)
	}

	return w.buf, w.err
}

// Raw appends json text the generator wrote: a member name with its
// punctuation.
func (w *Writer) Raw(s string) {
	w.buf = append(w.buf, s...)
}

// Encoded is one value already encoded as json, which a generated encoder
// appends as it is: a member whose shape the service's codegen options name
// as pre-encoded has an Encoded sibling (docs/architecture.md, "Pre-encoded
// values"). The bytes are the caller's and never checked: they must be exactly
// one json value, written as encoding/json/v2 would write the typed value.
// Empty is no value: null inside a list or a map, and an absent member.
type Encoded []byte

// EncodedMember appends the name of a member written from its encoded sibling,
// a json text as Raw takes, and marks the value as holding one: see Result.
func (w *Writer) EncodedMember(s string) {
	w.encoded = true
	w.buf = append(w.buf, s...)
}

// Encoded appends v as it is, and null for an empty v. A buffer too small for
// v doubles: the values of a list come one after another, and append's growth
// by a quarter would copy a long list's buffer several times more.
func (w *Writer) Encoded(v Encoded) {
	if len(v) == 0 {
		w.Null()

		return
	}

	if len(v) > cap(w.buf)-len(w.buf) {
		w.buf = slices.Grow(w.buf, max(len(v), cap(w.buf)))
	}

	w.buf = append(w.buf, v...)
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

	w.buf, ok = appendStrictQuote(w.buf, s)
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

// decimalBase and float64Bits are the base of an integer and the width of a
// float as json writes them.
const (
	decimalBase = 10
	float64Bits = 64
)
