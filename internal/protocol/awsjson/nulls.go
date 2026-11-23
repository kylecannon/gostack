package awsjson

import (
	"bytes"
	"encoding/binary"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"math/bits"
	"reflect"
)

// A json null inside a list or a map is not a value the type can hold: v2 reads
// it as the zero value, so ["a", null] and ["a", ""] reach a handler as the same
// slice. The decoders below leave a null list element, and a map entry whose
// value is null, out of the result, so a handler never receives a zero value the
// client did not send. botocore-generated clients never send one, and
// LocalStack's own handlers drop it.
//
// Only a list or map of scalars is covered: those are the collections whose
// zero value is a real value too (""), where a null in a list of structures is
// an empty structure that every handler already refuses for its missing
// members, and a timestamp element is a pointer, which keeps a null as nil.
// The generator makes no other scalar element type (scalarTypes in
// codegen/cmd/gostack-codegen/model.go); nulls_internal_test.go in that
// package checks it against DropsNulls.

// nullToken is the json null.
var nullToken = []byte("null")

// DecodeOption changes how a decode reads a body.
type DecodeOption struct{ keepNulls bool }

// KeepNulls makes a decode leave a null in a list or map to v2's default
// decoding, which reads it as the zero value, instead of dropping it. A service
// whose real implementation tells a null from an absent entry takes it
// (serviceOptions.KeepNulls in the generator).
var KeepNulls = DecodeOption{keepNulls: true}

func keepsNulls(opts []DecodeOption) bool {
	for _, o := range opts {
		if o.keepNulls {
			return true
		}
	}

	return false
}

// dropper is a list or map type whose nulls are dropped, with the unmarshaler
// that does it.
type dropper struct {
	typ reflect.Type
	un  *json.Unmarshalers
}

func listDropper[E any]() dropper {
	return dropper{reflect.TypeFor[[]E](), json.UnmarshalFromFunc(dropNullList[E])}
}

func mapDropper[E any]() dropper {
	return dropper{reflect.TypeFor[map[string]E](), json.UnmarshalFromFunc(dropNullMap[E])}
}

var droppers = []dropper{
	listDropper[string](), listDropper[int32](), listDropper[int64](), listDropper[float64](),
	listDropper[bool](), listDropper[[]byte](),
	mapDropper[string](), mapDropper[int32](), mapDropper[int64](), mapDropper[float64](),
	mapDropper[bool](), mapDropper[[]byte](),
}

// DropsNulls reports whether a decode leaves a null out of a value of type t: a
// list or a map with string keys of a string, number, boolean or blob.
func DropsNulls(t reflect.Type) bool {
	for _, d := range droppers {
		if d.typ == t {
			return true
		}
	}

	return false
}

// nullDroppers is every dropper as one set of unmarshalers.
var nullDroppers = func() *json.Unmarshalers {
	all := make([]*json.Unmarshalers, len(droppers))
	for i, d := range droppers {
		all[i] = d.un
	}

	return json.JoinUnmarshalers(all...)
}()

// strictNulls is strict for a body that holds a null, which is the only body
// the droppers can change. They cost a lookup on every value decoded, as
// lenient's readers do, so every other body goes without them.
var strictNulls = json.JoinOptions(strict, json.WithUnmarshalers(nullDroppers))

// strictFor is the strict settings for decoding body.
//
//nolint:ireturn // json.Options is the library's interface, and strict and strictNulls are both of it
func strictFor(body []byte) json.Options {
	if hasNullToken(body) {
		return strictNulls
	}

	return strict
}

// nullLookaround is how many bytes hasNullToken reads on either side of a null
// to place it, and nullChecks is how many nulls it places before it stops and
// counts the body as holding one.
const (
	nullLookaround = 64
	nullChecks     = 8
)

// hasNullToken reports whether body may have a json null outside a string, which
// is the only null the droppers can drop. The text null inside a string, as in a
// message body that is json itself, is no null for them, and must not make the
// body pay for them.
//
// It never says no for a body that has a null token. It says no only when what
// surrounds each null, in a few bytes, shows it to be text in a string: no
// delimiter follows it, no delimiter or one in a string precedes it, or the
// quote before it is escaped, which only a string has. Any null it cannot place
// that way counts, and so do the nulls past the eighth, so that a body that
// defeats it costs what it cost before (the droppers) and no more.
func hasNullToken(body []byte) bool {
	off := 0

	for checked := 0; ; checked++ {
		i := indexNull(body[off:])
		if i < 0 {
			return false
		}

		if checked == nullChecks {
			return true
		}

		p := off + i
		if !isStringText(body, p) {
			return true
		}

		off = p + len(nullToken)
	}
}

// indexNull is bytes.Index(b, nullToken): the index of the first null in b, or
// -1. It looks for the u, eight bytes to a step, and checks what surrounds each,
// where bytes.Index on arm64 has no vector search for a needle of four bytes and
// calls IndexByte for each n of the body, which costs more than the decode of a
// small body gains from the pass it gates.
func indexNull(b []byte) int {
	const (
		ones = 0x0101010101010101
		low7 = 0x7f7f7f7f7f7f7f7f
	)

	at := func(u int) int { // the index of a null whose u is at b[u], or -1
		if u >= 1 && u+3 <= len(b) && b[u-1] == 'n' && b[u+1] == 'l' && b[u+2] == 'l' {
			return u - 1
		}

		return -1
	}

	i := 0

	for ; i+8 <= len(b); i += 8 {
		x := binary.LittleEndian.Uint64(b[i:]) ^ ('u' * ones) // a zero byte where there is a u
		m := ^(((x & low7) + low7) | x | low7)                // 0x80 in each zero byte, and in no other

		for ; m != 0; m &= m - 1 {
			if j := at(i + bits.TrailingZeros64(m)>>3); j >= 0 {
				return j
			}
		}
	}

	for ; i < len(b); i++ {
		if b[i] == 'u' {
			if j := at(i); j >= 0 {
				return j
			}
		}
	}

	return -1
}

// isStringText reports whether the null at body[p:] is text inside a string of
// the json body. A json null is followed by a comma, a bracket or the end, and
// preceded by a colon, a comma or a bracket, white space aside. Where those do
// not hold the text is in a string, and where they hold it is not known unless
// the nearest quote before it is escaped, which only a string holds: the null is
// then in the string that quote is in, whatever the string's own text looks
// like.
func isStringText(body []byte, p int) bool {
	j := p + len(nullToken)
	for n := 0; j < len(body) && isSpace(body[j]); n++ {
		if n == nullLookaround {
			return false
		}

		j++
	}

	if j < len(body) && body[j] != ',' && body[j] != ']' && body[j] != '}' {
		return true
	}

	i := p - 1
	for n := 0; i >= 0 && isSpace(body[i]); n++ {
		if n == nullLookaround {
			return false
		}

		i--
	}

	if i < 0 {
		return false // the body starts with it
	}

	switch body[i] {
	case ':', ',', '[':
	default:
		return true // a quote, a backslash, a letter: no json null follows these
	}

	lo := max(0, i-nullLookaround)

	q := bytes.LastIndexByte(body[lo:i], '"')
	if q < 0 {
		return false
	}

	backslashes := 0
	for b := lo + q - 1; b >= 0 && body[b] == '\\'; b-- {
		backslashes++
	}

	return backslashes%2 == 1
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// peekKind is the kind of the next token, or the decoder's error when there is
// none.
func peekKind(dec *jsontext.Decoder) (jsontext.Kind, error) {
	k := dec.PeekKind()
	if k != 0 {
		return k, nil
	}

	_, err := dec.ReadToken()
	if err == nil {
		err = errNoValue
	}

	return 0, err
}

var errNoValue = errors.New("no json value where one is expected")

// dropNullList reads a json array into v without its null elements. A value that
// is not an array is left to the default decoding.
func dropNullList[E any](dec *jsontext.Decoder, v *[]E) error {
	if dec.PeekKind() != '[' {
		return errors.ErrUnsupported
	}

	_, err := dec.ReadToken()
	if err != nil {
		return err //nolint:wrapcheck // an unmarshaler returns the decoder's error as is
	}

	out := []E{} // an empty array is an empty slice, as the default decoding reads it

	for {
		k, err := peekKind(dec)
		if err != nil {
			return err
		}

		switch k {
		case ']':
			_, err = dec.ReadToken()
			*v = out

			return err //nolint:wrapcheck // an unmarshaler returns the decoder's error as is
		case 'n':
			_, err = dec.ReadToken()
			if err != nil {
				return err //nolint:wrapcheck // as above
			}
		default:
			var e E

			err = json.UnmarshalDecode(dec, &e)
			if err != nil {
				return err //nolint:wrapcheck // as above
			}

			out = append(out, e)
		}
	}
}

// dropNullMap reads a json object into v without the members whose value is
// null. It adds to the map v already holds, as v2's default decoding does for a
// member that repeats. A name that repeats keeps its last value, so a null after
// a value removes the entry.
func dropNullMap[E any](dec *jsontext.Decoder, v *map[string]E) error {
	if dec.PeekKind() != '{' {
		return errors.ErrUnsupported
	}

	_, err := dec.ReadToken()
	if err != nil {
		return err //nolint:wrapcheck // an unmarshaler returns the decoder's error as is
	}

	out := *v // a member that repeats adds to the map it set, as v2 does
	if out == nil {
		out = map[string]E{}
	}

	for {
		k, err := peekKind(dec)
		if err != nil {
			return err
		}

		if k == '}' {
			_, err = dec.ReadToken()
			*v = out

			return err //nolint:wrapcheck // as above
		}

		tok, err := dec.ReadToken()
		if err != nil {
			return err //nolint:wrapcheck // as above
		}

		name := tok.String() // a token is void once the decoder reads on

		k, err = peekKind(dec)
		if err != nil {
			return err
		}

		if k == 'n' {
			_, err = dec.ReadToken()
			if err != nil {
				return err //nolint:wrapcheck // as above
			}

			delete(out, name)

			continue
		}

		var e E

		err = json.UnmarshalDecode(dec, &e)
		if err != nil {
			return err //nolint:wrapcheck // as above
		}

		out[name] = e
	}
}
