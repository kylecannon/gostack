package awsjsontest

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"reflect"
	"slices"
	"strings"
)

// Equal reports whether a and b, both json of a value of type typ, are the
// same byte for byte once the members of each object that is a Go map are
// sorted: v2 writes a map in Go's map order, which changes from run to run.
// Every struct's members must come in the same order, and every name, string,
// number and literal must match as written, escapes included.
func Equal(a, b []byte, typ reflect.Type) bool {
	if bytes.Equal(a, b) {
		return true
	}

	ca, errA := canonical(a, typ)
	cb, errB := canonical(b, typ)

	return errA == nil && errB == nil && bytes.Equal(ca, cb)
}

// canonical returns b, json of a value of type typ, with the members of each
// map's object sorted by their names as written, and nothing else changed.
func canonical(b []byte, typ reflect.Type) ([]byte, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(b))

	out, err := canonicalValue(nil, dec, typ)
	if err != nil {
		return nil, err
	}

	if dec.PeekKind() != 0 {
		return nil, errors.New("json after the value")
	}

	return out, nil
}

// errShape is json that does not have the shape of its Go type.
var errShape = errors.New("json does not match its type")

func canonicalValue(out []byte, dec *jsontext.Decoder, typ reflect.Type) ([]byte, error) {
	for typ.Kind() == reflect.Pointer && typ.Elem() != timeType {
		typ = typ.Elem()
	}

	kind := dec.PeekKind()

	switch {
	case kind == 'n' || typ == valueType || typ == timeType || typ.Kind() == reflect.Pointer:
		return readRaw(out, dec)
	case typ.Kind() == reflect.Struct && kind == '{':
		return canonicalStruct(out, dec, typ)
	case typ.Kind() == reflect.Map && kind == '{':
		return canonicalMap(out, dec, typ)
	case typ.Kind() == reflect.Slice && typ.Elem().Kind() != reflect.Uint8 && kind == '[':
		return canonicalList(out, dec, typ)
	case typ.Kind() == reflect.Struct, typ.Kind() == reflect.Map,
		typ.Kind() == reflect.Slice && typ.Elem().Kind() != reflect.Uint8:
		return nil, errShape
	default:
		return readRaw(out, dec)
	}
}

func readRaw(out []byte, dec *jsontext.Decoder) ([]byte, error) {
	v, err := dec.ReadValue()

	return append(out, v...), err
}

func canonicalList(out []byte, dec *jsontext.Decoder, typ reflect.Type) ([]byte, error) {
	_, err := dec.ReadToken()
	if err != nil {
		return nil, err //nolint:wrapcheck // the test reports it
	}

	out = append(out, '[')

	for i := 0; dec.PeekKind() != ']'; i++ {
		if i > 0 {
			out = append(out, ',')
		}

		out, err = canonicalValue(out, dec, typ.Elem())
		if err != nil {
			return nil, err
		}
	}

	_, err = dec.ReadToken()

	return append(out, ']'), err
}

func canonicalStruct(out []byte, dec *jsontext.Decoder, typ reflect.Type) ([]byte, error) {
	_, err := dec.ReadToken()
	if err != nil {
		return nil, err //nolint:wrapcheck // the test reports it
	}

	out = append(out, '{')

	for i := 0; dec.PeekKind() != '}'; i++ {
		name, nameErr := dec.ReadValue()
		if nameErr != nil {
			return nil, nameErr //nolint:wrapcheck // the test reports it
		}

		field, ok := fieldByJSONName(typ, string(name))
		if !ok {
			return nil, errors.New("no member " + string(name) + " in " + typ.String())
		}

		if i > 0 {
			out = append(out, ',')
		}

		out = append(append(out, name...), ':')

		out, err = canonicalValue(out, dec, field.Type)
		if err != nil {
			return nil, err
		}
	}

	_, err = dec.ReadToken()

	return append(out, '}'), err
}

func canonicalMap(out []byte, dec *jsontext.Decoder, typ reflect.Type) ([]byte, error) {
	_, err := dec.ReadToken()
	if err != nil {
		return nil, err //nolint:wrapcheck // the test reports it
	}

	var members [][]byte

	for dec.PeekKind() != '}' {
		name, nameErr := dec.ReadValue()
		if nameErr != nil {
			return nil, nameErr //nolint:wrapcheck // the test reports it
		}

		member := append(slices.Clone([]byte(name)), ':')

		member, err = canonicalValue(member, dec, typ.Elem())
		if err != nil {
			return nil, err
		}

		members = append(members, member)
	}

	_, err = dec.ReadToken()
	if err != nil {
		return nil, err //nolint:wrapcheck // the test reports it
	}

	slices.SortFunc(members, bytes.Compare)

	out = append(out, '{')
	out = append(out, bytes.Join(members, []byte{','})...)

	return append(out, '}'), nil
}

// fieldByJSONName returns the field of the struct type typ whose json name,
// quoted, is name.
func fieldByJSONName(typ reflect.Type, name string) (reflect.StructField, bool) {
	for f := range typ.Fields() {
		if `"`+jsonName(f)+`"` == name {
			return f, true
		}
	}

	return reflect.StructField{}, false
}

// jsonName is the member name of f: its json tag's name, or the field's.
func jsonName(f reflect.StructField) string {
	tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	if tag == "" {
		return f.Name
	}

	return tag
}
