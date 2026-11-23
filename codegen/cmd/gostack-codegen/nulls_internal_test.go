package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol/awsjson"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nullElems are the Go types a list or map of scalars can hold, as scalarTypes
// writes them. awsjson drops a null from each (awsjson.DropsNulls): without it
// a handler cannot tell a null element from a zero one.
var nullElems = map[string]reflect.Type{
	"string":  reflect.TypeFor[string](),
	"int32":   reflect.TypeFor[int32](),
	"int64":   reflect.TypeFor[int64](),
	"float64": reflect.TypeFor[float64](),
	"bool":    reflect.TypeFor[bool](),
	"[]byte":  reflect.TypeFor[[]byte](),
}

// A timestamp element is a pointer, which keeps a null as nil.
const timeElem = "*protocol.Time"

func Test_scalarTypes_NullsAreDropped(t *testing.T) {
	t.Parallel()

	for shape, goType := range scalarTypes {
		if goType == timeElem {
			continue
		}

		elem, ok := nullElems[goType]
		require.True(t, ok, "the %s element type %s needs a null dropper in awsjson/nulls.go", shape, goType)
		assert.True(t, awsjson.DropsNulls(reflect.SliceOf(elem)), "[]%s", goType)
		assert.True(t, awsjson.DropsNulls(reflect.MapOf(reflect.TypeFor[string](), elem)), "map[string]%s", goType)
	}
}

// Every list and map of scalars a generated type holds is one awsjson drops a
// null from, so no handler is given a zero value the client did not send.
func Test_generated_collections_DropNulls(t *testing.T) {
	t.Parallel()

	loader := newTestLoader(t)

	var seen int

	for name := range serviceTable {
		g, err := newGenerator(name, loader)
		require.NoError(t, err, name)

		if !g.hasProtocol(protoJSON) && !g.hasProtocol(protoRestJSON) {
			continue
		}

		for _, st := range g.sortedStructs() {
			for _, f := range st.fields {
				if !inJSONBody(st, f) {
					continue
				}

				elem, kind := collectionOf(f.typ)
				if kind == "" || elem == timeElem {
					continue
				}

				typ, ok := nullElems[elem]
				if !ok {
					continue // a collection of collections or structures
				}

				if kind == "list" {
					typ = reflect.SliceOf(typ)
				} else {
					typ = reflect.MapOf(reflect.TypeFor[string](), typ)
				}

				seen++

				assert.True(t, awsjson.DropsNulls(typ), "%s.%s is a %s", name, f.goName, f.typ)
			}
		}
	}

	assert.Positive(t, seen, "the json services have lists of strings")
}

// collectionOf splits the Go type of a list or map into its element type and
// "list" or "map", or returns "" for the kind of any other type.
func collectionOf(typ string) (elem, kind string) {
	if e, ok := strings.CutPrefix(typ, "[]"); ok && typ != "[]byte" {
		return e, "list"
	}

	if e, ok := strings.CutPrefix(typ, "map[string]"); ok {
		return e, "map"
	}

	return "", ""
}
