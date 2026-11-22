package awsjsontest

import (
	"errors"
	"reflect"
)

// errBothRefused is a value both sides refuse alike.
var errBothRefused = errors.New("both refused the value")

// reachableMembers adds to out the members, as Type.Field, that the filler can
// set in a value of the output type typ, a pointer to a struct. It follows the
// filler's depth rule: a pointer, list or map is nil from maxDepth down, and a
// time or a document is set at any depth.
func reachableMembers(typ reflect.Type, out map[string]bool) {
	walkMembers(typ.Elem(), 0, out, map[depthKey]bool{})
}

type depthKey struct {
	typ   reflect.Type
	depth int
}

func walkMembers(typ reflect.Type, depth int, out map[string]bool, seen map[depthKey]bool) {
	if seen[depthKey{typ, depth}] || typ == valueType || typ == timeType {
		return
	}

	seen[depthKey{typ, depth}] = true

	switch typ.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map:
		if typ.Elem() != timeType && depth < maxDepth {
			walkMembers(typ.Elem(), depth+1, out, seen)
		}
	case reflect.Struct:
		for f := range typ.Fields() {
			if settable(f.Type, depth) && !holdsEncoded(f.Type) {
				out[typ.Name()+"."+f.Name] = true
				walkMembers(f.Type, depth, out, seen)
			}
		}
	default:
	}
}

// settable reports whether the filler can set a member of type typ in a
// struct at depth.
func settable(typ reflect.Type, depth int) bool {
	switch {
	case typ == valueType, typ.Kind() == reflect.Pointer && typ.Elem() == timeType:
		return true
	case typ.Kind() == reflect.Pointer, typ.Kind() == reflect.Slice, typ.Kind() == reflect.Map:
		return depth < maxDepth
	default:
		return true
	}
}

// markSet adds to set the members, as Type.Field, that are set in v: not a nil
// pointer, list or map.
func markSet(v reflect.Value, set map[string]bool) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() && v.Type().Elem() != timeType {
			markSet(v.Elem(), set)
		}
	case reflect.Slice:
		if v.Type() == valueType || v.Type().Elem().Kind() == reflect.Uint8 {
			return
		}

		for i := range v.Len() {
			markSet(v.Index(i), set)
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			markSet(it.Value(), set)
		}
	case reflect.Struct:
		if v.Type() == timeType {
			return
		}

		for i := range v.NumField() {
			f := v.Field(i)
			if (f.Kind() == reflect.Pointer || f.Kind() == reflect.Slice || f.Kind() == reflect.Map) && f.IsNil() {
				continue
			}

			set[v.Type().Name()+"."+v.Type().Field(i).Name] = true
			markSet(f, set)
		}
	default:
	}
}
