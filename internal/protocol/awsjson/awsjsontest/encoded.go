package awsjsontest

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol/awsjson"
)

// encodedSuffix ends the name of a member's encoded sibling, as the generator
// names it.
const encodedSuffix = "JSON"

var encodedType = reflect.TypeFor[awsjson.Encoded]()

// holdsEncoded reports whether typ is awsjson.Encoded or a list or map of it:
// the type of an encoded sibling.
func holdsEncoded(typ reflect.Type) bool {
	for typ != encodedType && (typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map) {
		typ = typ.Elem()
	}

	return typ == encodedType
}

// CheckEncoded compares, on values of each output type, the generated encoder
// writing every member that has an encoded sibling from that sibling with v2
// writing the member itself. Each value is random, its strings valid UTF-8,
// from a fixed seed. Before the second write each such member is moved into
// its sibling, each of its encoded values marshaled by v2 on its own, and the
// member is cleared, so only the sibling can put it on the wire. It fails when
// no output has such a member, or when one was never set to move.
func CheckEncoded(t *testing.T, outputs map[string]any, n int) {
	t.Helper()

	rng := rand.NewChaCha8([32]byte{1})
	data := make([]byte, dataSize)
	full := bytes.Repeat([]byte{fullByte}, fullSize)
	seen := map[string]bool{}
	moved := map[string]bool{}

	for _, op := range sortedKeys(outputs) {
		check := func(data []byte) {
			err := compareEncoded(outputs[op], data, seen, moved)
			if err != nil {
				t.Fatalf("%s: %v", op, err)
			}
		}

		check(full)

		for range n {
			_, _ = rng.Read(data)
			check(data)
		}
	}

	if len(seen) == 0 {
		t.Fatal("no output has a member with an encoded sibling")
	}

	for _, member := range sortedKeys(seen) {
		if !moved[member] {
			t.Errorf("member %s was never set to move into its encoded sibling", member)
		}
	}
}

func compareEncoded(out any, data []byte, seen, moved map[string]bool) error {
	typ := reflect.TypeOf(out)
	if typ.Kind() != reflect.Pointer {
		return fmt.Errorf("output %T is not a pointer", out)
	}

	v := reflect.New(typ.Elem())
	(&filler{data: data, valid: true}).fill(v.Elem(), 0)

	a, ok := reflect.TypeAssert[awsjson.Appender](v)
	if !ok {
		return fmt.Errorf("output %T has no generated json encoder", out)
	}

	want, err := awsjson.MarshalReflect(v.Interface())
	if err != nil {
		return fmt.Errorf("v2 refused a value with valid UTF-8: %w", err)
	}

	err = moveEncoded(v, seen, moved)
	if err != nil {
		return err
	}

	got, err := a.AppendJSON(nil)
	if err != nil {
		return fmt.Errorf("the encoder refused the value with encoded members: %w", err)
	}

	if !Equal(got, want, typ) {
		return fmt.Errorf("json differs\n got %s\nwant %s", got, want)
	}

	return nil
}

// moveEncoded moves every member below v that has an encoded sibling into it,
// and records each such member in seen and each one it moved in moved.
func moveEncoded(v reflect.Value, seen, moved map[string]bool) error {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() && v.Type().Elem() != timeType {
			return moveEncoded(v.Elem(), seen, moved)
		}
	case reflect.Slice:
		if v.Type() == valueType || v.Type().Elem().Kind() == reflect.Uint8 {
			return nil
		}

		for i := range v.Len() {
			err := moveEncoded(v.Index(i), seen, moved)
			if err != nil {
				return err
			}
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			// A map value cannot be changed in place: move a copy and store it.
			e := reflect.New(v.Type().Elem()).Elem()
			e.Set(it.Value())

			err := moveEncoded(e, seen, moved)
			if err != nil {
				return err
			}

			v.SetMapIndex(it.Key(), e)
		}
	case reflect.Struct:
		if v.Type() == timeType {
			return nil
		}

		return moveStruct(v, seen, moved)
	default:
	}

	return nil
}

func moveStruct(v reflect.Value, seen, moved map[string]bool) error {
	typ := v.Type()

	for i := range v.NumField() {
		f, sf := v.Field(i), typ.Field(i)
		if holdsEncoded(sf.Type) {
			continue
		}

		sib, ok := typ.FieldByName(sf.Name + encodedSuffix)
		if !ok || !holdsEncoded(sib.Type) {
			err := moveEncoded(f, seen, moved)
			if err != nil {
				return err
			}

			continue
		}

		member := typ.Name() + "." + sf.Name
		seen[member] = true

		if isNil(f) {
			continue
		}

		enc, err := toEncoded(f, sib.Type)
		if err != nil {
			return fmt.Errorf("%s: %w", member, err)
		}

		v.FieldByIndex(sib.Index).Set(enc)
		f.SetZero()

		moved[member] = true
	}

	return nil
}

// errNotCollection is a typed value whose sibling type is a list or map when
// the value is not.
var errNotCollection = errors.New("the member and its encoded sibling differ in shape")

// isNil reports whether v is a nil pointer, list or map.
func isNil(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map:
		return v.IsNil()
	default:
		return false
	}
}

// toEncoded returns v as a value of typ, its encoded sibling's type: each
// value at an awsjson.Encoded is v2's json of it, and a nil one stays nil.
func toEncoded(v reflect.Value, typ reflect.Type) (reflect.Value, error) {
	if isNil(v) {
		return reflect.Zero(typ), nil
	}

	switch {
	case typ == encodedType:
		b, err := awsjson.MarshalReflect(v.Interface())
		if err != nil {
			return reflect.Value{}, fmt.Errorf("marshal an encoded value: %w", err)
		}

		return reflect.ValueOf(awsjson.Encoded(b)), nil
	case typ.Kind() == reflect.Slice && v.Kind() == reflect.Slice:
		out := reflect.MakeSlice(typ, v.Len(), v.Len())

		for i := range v.Len() {
			e, err := toEncoded(v.Index(i), typ.Elem())
			if err != nil {
				return reflect.Value{}, err
			}

			out.Index(i).Set(e)
		}

		return out, nil
	case typ.Kind() == reflect.Map && v.Kind() == reflect.Map:
		out := reflect.MakeMapWithSize(typ, v.Len())

		for it := v.MapRange(); it.Next(); {
			e, err := toEncoded(it.Value(), typ.Elem())
			if err != nil {
				return reflect.Value{}, err
			}

			out.SetMapIndex(it.Key().Convert(typ.Key()), e)
		}

		return out, nil
	default:
		return reflect.Value{}, fmt.Errorf("%w: %s and %s", errNotCollection, v.Type(), typ)
	}
}
