package rest_test

import (
	"encoding/json/jsontext"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/kylecannon/gostack/internal/protocol"
	jsongen "github.com/kylecannon/gostack/internal/protocol/restjson/fixturegen_test"
	xmlgen "github.com/kylecannon/gostack/internal/protocol/restxml/fixturegen_test"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/stretchr/testify/require"
)

// The fixture drivers run each case through the service gostack-codegen
// generated from the case's group (make gen writes them, with -fixtures, under
// internal/protocol/<protocol>/fixturegen_test). An input case serves the
// serialized request, which the generated router sends to the operation, and
// compares the input the generated reader decoded with params. An output case
// fills the generated output from result, or makes the generated error, and
// parses what the generated writer sent as botocore does. Converting between
// the fixture's json and the generated Go types is the harness's own, by
// reflection over the group's shapes.

// generatedGroup returns the generated service of group i of a fixture file.
func generatedGroup(t *testing.T, proto, kind string, i int) xmlgen.Group {
	t.Helper()

	groups := map[string][]xmlgen.Group{
		"rest-xml/input": xmlgen.Input, "rest-xml/output": xmlgen.Output,
		"rest-json/input": convertGroups(jsongen.Input), "rest-json/output": convertGroups(jsongen.Output),
	}[proto+"/"+kind]

	require.Less(t, i, len(groups), "no generated service for group %d of %s/%s: run make gen", i, kind, proto)

	return groups[i]
}

func convertGroups(in []jsongen.Group) []xmlgen.Group {
	out := make([]xmlgen.Group, 0, len(in))
	for _, g := range in {
		out = append(out, xmlgen.Group(g))
	}

	return out
}

// serveGenerated serves req with the generated service, answering with out or
// err, and returns the response and the input the service decoded. An empty op
// leaves finding the operation to the generated router.
func serveGenerated(
	gen xmlgen.Group, req *http.Request, op string, out any, err error,
) (*httptest.ResponseRecorder, any) {
	rec := httptest.NewRecorder()
	rc := &request.Context{Request: req, Protocol: gen.Metadata.Protocols[0], RequestID: requestID, Operation: op}

	in := gen.Run(rec, rc, out, err)

	return rec, in
}

// exportName is the generator's name for the Go field of a member.
func exportName(name string) string {
	r, size := utf8.DecodeRuneInString(name)

	return string(unicode.ToUpper(r)) + name[size:]
}

var (
	timeType     = reflect.TypeFor[*protocol.Time]()
	documentType = reflect.TypeFor[jsontext.Value]()
	readerType   = reflect.TypeFor[io.Reader]()
)

// fromGo turns a generated value into the form a fixture holds, as plain does
// for the runtime's values.
func fromGo(t *testing.T, m model, mm member, v reflect.Value) any {
	t.Helper()

	if !v.IsValid() || (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface ||
		v.Kind() == reflect.Slice || v.Kind() == reflect.Map) && v.IsNil() {
		return nil
	}

	switch {
	case v.Type() == timeType:
		return plain(v.Interface())
	case v.Type() == documentType:
		return jsonDynamic(t, v.Bytes())
	case v.Type().Implements(readerType) || v.Kind() == reflect.Interface:
		data, err := io.ReadAll(v.Interface().(io.Reader)) //nolint:forcetypeassert // the case checks it
		require.NoError(t, err)

		return string(data)
	case mm.jsonvalue:
		return jsonDynamic(t, []byte(v.Elem().String()))
	}

	switch mm.shape.Type {
	case typeStructure:
		out := map[string]any{}

		for _, f := range m.members(mm.shape) {
			if x := fromGo(t, m, f, v.Elem().FieldByName(exportName(f.name))); x != nil {
				out[f.name] = x
			}
		}

		return out
	case typeList:
		el := m.resolve("", mm.shape.Member)
		out := make([]any, 0, v.Len())

		for i := range v.Len() {
			out = append(out, fromGo(t, m, el, addr(v.Index(i))))
		}

		return out
	case typeMap:
		val := m.resolve("", mm.shape.Value)
		out := make(map[string]any, v.Len())

		for _, k := range v.MapKeys() {
			out[k.String()] = fromGo(t, m, val, addr(v.MapIndex(k)))
		}

		return out
	default:
		return plain(v.Interface())
	}
}

// addr turns a struct element into a pointer to a copy of it, the form a
// struct member has, so fromGo reads both alike.
func addr(v reflect.Value) reflect.Value {
	if v.Kind() != reflect.Struct || v.Type() == timeType.Elem() {
		return v
	}

	p := reflect.New(v.Type())
	p.Elem().Set(v)

	return p
}

// toGo fills dst, a generated value, from v, a value of the fixture's json.
func toGo(t *testing.T, m model, mm member, v any, dst reflect.Value) {
	t.Helper()

	if v == nil {
		return
	}

	switch {
	case dst.Type() == timeType:
		dst.Set(reflect.ValueOf(timeOf(t, v)))

		return
	case dst.Type() == documentType:
		dst.SetBytes(marshalJSON(t, v))

		return
	case dst.Kind() == reflect.Interface:
		dst.Set(reflect.ValueOf(io.NopCloser(strings.NewReader(must[string](t, v)))))

		return
	case dst.Kind() == reflect.Pointer && mm.shape.Type != typeStructure:
		dst.Set(reflect.New(dst.Type().Elem()))
		toGo(t, m, mm, v, dst.Elem())

		return
	}

	switch mm.shape.Type {
	case typeStructure:
		if dst.Kind() == reflect.Pointer {
			dst.Set(reflect.New(dst.Type().Elem()))
			dst = dst.Elem()
		}

		fields := must[map[string]any](t, v)
		for _, f := range m.members(mm.shape) {
			// An error's message is not a member of its generated struct.
			if x, ok := fields[f.name]; ok && dst.FieldByName(exportName(f.name)).IsValid() {
				toGo(t, m, f, x, dst.FieldByName(exportName(f.name)))
			}
		}
	case typeList:
		items := must[[]any](t, v)
		el := m.resolve("", mm.shape.Member)
		dst.Set(reflect.MakeSlice(dst.Type(), len(items), len(items)))

		for i, item := range items {
			toGo(t, m, el, item, dst.Index(i))
		}
	case typeMap:
		entries := must[map[string]any](t, v)
		val := m.resolve("", mm.shape.Value)
		dst.Set(reflect.MakeMapWithSize(dst.Type(), len(entries)))

		for k, e := range entries {
			x := reflect.New(dst.Type().Elem()).Elem()
			toGo(t, m, val, e, x)
			dst.SetMapIndex(reflect.ValueOf(k), x)
		}
	default:
		toGoScalar(t, mm, v, dst)
	}
}

func toGoScalar(t *testing.T, mm member, v any, dst reflect.Value) {
	t.Helper()

	switch mm.shape.Type {
	case typeString:
		if mm.jsonvalue {
			dst.SetString(string(marshalJSON(t, v)))

			return
		}

		dst.SetString(must[string](t, v))
	case typeInteger, typeLong:
		dst.SetInt(int64(number(t, v)))
	case typeFloat, typeDouble:
		dst.SetFloat(number(t, v))
	case typeBoolean:
		dst.SetBool(must[bool](t, v))
	case typeBlob:
		dst.SetBytes([]byte(must[string](t, v)))
	default:
		panic("scalar of type " + mm.shape.Type)
	}
}
