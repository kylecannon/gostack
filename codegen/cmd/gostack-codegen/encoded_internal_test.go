package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/codegen/spec"
	"github.com/stretchr/testify/require"
)

// encodedModel is a json service whose output holds an ItemMap as a member,
// in a list and in a map of lists, and in a nested structure. %s is the
// protocol member of the metadata, %s more members of GetResponse and %s more
// shapes.
const encodedModel = `{"metadata":{%s,"jsonVersion":"1.0","targetPrefix":"Enc",
	"apiVersion":"2020-01-01","endpointPrefix":"enc"},
	"operations":{"Get":{"name":"Get","http":{"method":"POST","requestUri":"/"},
		"input":{"shape":"GetRequest"},"output":{"shape":"GetResponse"}}},
	"shapes":{
		"String":{"type":"string"},
		"Value":{"type":"structure","members":{"S":{"shape":"String"}}},
		"ItemMap":{"type":"map","key":{"shape":"String"},"value":{"shape":"Value"}},
		"ItemList":{"type":"list","member":{"shape":"ItemMap"}},
		"TableMap":{"type":"map","key":{"shape":"String"},"value":{"shape":"ItemList"}},
		"Wrapped":{"type":"structure","members":{"Item":{"shape":"ItemMap"}}},
		"GetRequest":{"type":"structure","members":{"Key":{"shape":"ItemMap"}}},
		"GetResponse":{"type":"structure","members":{"Item":{"shape":"ItemMap"},"Items":{"shape":"ItemList"},
			"Tables":{"shape":"TableMap"},"Wrapped":{"shape":"Wrapped"},"Name":{"shape":"String"}%s}}%s}}`

// itemMap is the shape of encodedModel the tests encode.
const itemMap = "ItemMap"

// encodedGenerator builds encodedModel. protocol is one protocol, or a json
// list of them.
func encodedGenerator(t *testing.T, protocol, members, shapes string, opts serviceOptions) (*generator, error) {
	t.Helper()

	meta := fmt.Sprintf(`"protocol":%q`, protocol)
	if strings.HasPrefix(protocol, "[") {
		meta = `"protocols":` + protocol
	}

	svc, err := spec.ParseBytes(fmt.Appendf(nil, encodedModel, meta, members, shapes))
	require.NoError(t, err)

	return newModel("enc", opts, svc, nil)
}

func Test_markEncoded(t *testing.T) {
	t.Parallel()

	g, err := encodedGenerator(t, protoJSON, "", "", serviceOptions{EncodedShapes: []string{itemMap}})
	require.NoError(t, err)

	siblings := map[string]string{}

	for _, st := range g.sortedStructs() {
		for _, f := range st.fields {
			if f.encoded != nil {
				siblings[st.goName+"."+f.encoded.goName] = f.encoded.typ
			}
		}
	}

	// GetRequest.Key is input only: decoding has no encoded values.
	require.Equal(t, map[string]string{
		"GetOutput.ItemJSON":   "awsjson.Encoded",
		"GetOutput.ItemsJSON":  "[]awsjson.Encoded",
		"GetOutput.TablesJSON": "map[string][]awsjson.Encoded",
		"Wrapped.ItemJSON":     "awsjson.Encoded",
	}, siblings)

	types, err := g.typesFile()
	require.NoError(t, err)
	require.Contains(t, types, "\tTablesJSON map[string][]awsjson.Encoded `json:\"-\"`\n")

	codec, err := g.jsonFile()
	require.NoError(t, err)
	require.Contains(t, codec, "\tif len(v.ItemJSON) != 0 {\n\t\tw.EncodedMember(`,\"Item\":`)\n"+
		"\t\tw.Encoded(v.ItemJSON)\n\t} else if v.Item != nil {\n")
	require.Contains(t, codec, "\tif v.ItemsJSON != nil {\n")
	require.Contains(t, codec, "w.Encoded(v.ItemsJSON[i0])")
	require.Contains(t, codec, "for k0, e0 := range v.TablesJSON {")
	require.Contains(t, codec, "w.Encoded(e0[i1])")

	test, err := g.jsonTestFile()
	require.NoError(t, err)
	require.Contains(t, test, "awsjsontest.CheckEncoded(t, jsonOutputs,")
}

// Without the option nothing changes: no sibling, no awsjson in the types,
// and no encoded test.
func Test_markEncoded_off(t *testing.T) {
	t.Parallel()

	g, err := encodedGenerator(t, protoJSON, "", "", serviceOptions{})
	require.NoError(t, err)

	types, err := g.typesFile()
	require.NoError(t, err)
	require.NotContains(t, types, "JSON ")

	codec, err := g.jsonFile()
	require.NoError(t, err)
	require.NotContains(t, codec, "Encoded")

	test, err := g.jsonTestFile()
	require.NoError(t, err)
	require.NotContains(t, test, "CheckEncoded")
}

// A struct v2 writes for its timestamps generates when no struct below it has
// an encoded member, though the output holding it has one.
func Test_markEncoded_timestamps(t *testing.T) {
	t.Parallel()

	stamp := `,"Stamp":{"type":"timestamp","timestampFormat":"iso8601"}`

	tests := []struct {
		name    string
		members string
		shapes  string
		holder  string
	}{
		{
			name: "struct with no sibling below", members: `,"Stamped":{"shape":"Stamped"}`,
			shapes: stamp + `,"Stamped":{"type":"structure","members":{"When":{"shape":"Stamp"},"Name":{"shape":"String"}}}`,
			holder: "Stamped",
		},
		{
			name: "self-recursive struct", members: `,"Node":{"shape":"Node"}`,
			shapes: stamp + `,"Node":{"type":"structure","members":{"Next":{"shape":"Node"},"When":{"shape":"Stamp"}}}`,
			holder: "Node",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			g, err := encodedGenerator(t, protoJSON, tt.members, tt.shapes,
				serviceOptions{EncodedShapes: []string{itemMap}})
			require.NoError(t, err)

			st := g.structs[tt.holder]
			require.NotNil(t, st)
			require.NotEmpty(t, timeFields(st))
			require.Empty(t, g.encodedHolder(st, map[string]bool{}))

			out := g.structs["GetOutput"]
			require.NotNil(t, out)
			require.True(t, slices.ContainsFunc(out.fields, func(f *field) bool { return f.encoded != nil }))
		})
	}
}

func Test_markEncoded_errors(t *testing.T) {
	t.Parallel()

	item := serviceOptions{EncodedShapes: []string{itemMap}}
	timestamp := `,"When":{"shape":"Stamp"}`

	tests := []struct {
		name     string
		protocol string
		members  string
		shapes   string
		opts     serviceOptions
		wantErr  string
	}{
		{
			name: "unknown shape", protocol: protoJSON, opts: serviceOptions{EncodedShapes: []string{"Missing"}},
			wantErr: "encoded shape Missing is not a shape",
		},
		{
			name: "unreached shape", protocol: protoJSON, opts: serviceOptions{EncodedShapes: []string{itemMap, "Spare"}},
			shapes:  `,"Spare":{"type":"map","key":{"shape":"String"},"value":{"shape":"String"}}`,
			wantErr: "encoded shape Spare is no member",
		},
		{
			name: "taken sibling name", protocol: protoJSON, opts: item, members: `,"ItemJSON":{"shape":"String"}`,
			wantErr: "the encoded sibling ItemJSON of member Item is taken",
		},
		{
			name: "struct v2 writes", protocol: protoJSON, opts: item, members: timestamp,
			shapes:  `,"Stamp":{"type":"timestamp","timestampFormat":"iso8601"}`,
			wantErr: "GetOutput: v2 writes the struct for its timestamps, and it has an encoded member",
		},
		{
			// Outer has no encoded member, but v2 writes the Wrapped in its list.
			name: "struct below one v2 writes", protocol: protoJSON, opts: item, members: `,"Outer":{"shape":"Outer"}`,
			shapes: `,"Stamp":{"type":"timestamp","timestampFormat":"iso8601"},` +
				`"WrappedList":{"type":"list","member":{"shape":"Wrapped"}},` +
				`"Outer":{"type":"structure","members":{"Many":{"shape":"WrappedList"},"When":{"shape":"Stamp"}}}`,
			wantErr: "Outer: v2 writes the struct for its timestamps, and Wrapped below it has an encoded member",
		},
		{name: "rest-json", protocol: protoRestJSON, opts: item, wantErr: "needs json as the only protocol"},
		{
			name: "json and query", protocol: `["json","query"]`, opts: item,
			wantErr: "needs json as the only protocol, not [json query]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := encodedGenerator(t, tt.protocol, tt.members, tt.shapes, tt.opts)
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}
