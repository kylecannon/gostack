package awsjson

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
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

// nullToken is what a body holds when it has a json null anywhere.
var nullToken = []byte("null")

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
	if bytes.Contains(body, nullToken) {
		return strictNulls
	}

	return strict
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
// null. A name that repeats keeps its last value, so a null after a value
// removes the entry.
func dropNullMap[E any](dec *jsontext.Decoder, v *map[string]E) error {
	if dec.PeekKind() != '{' {
		return errors.ErrUnsupported
	}

	_, err := dec.ReadToken()
	if err != nil {
		return err //nolint:wrapcheck // an unmarshaler returns the decoder's error as is
	}

	out := map[string]E{}

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
