package main

import (
	"fmt"
	"testing"

	"github.com/kylecannon/gostack/codegen/spec"
	"github.com/stretchr/testify/require"
)

// Test_queryFile_RecursiveInput: a query service whose input holds a structure
// of its own type below it, directly or through a list or map, fails generation,
// because the query decoder would recurse once per level of a request key. The
// same shapes in an output, or in a json service, generate.
func Test_queryFile_RecursiveInput(t *testing.T) {
	t.Parallel()

	const (
		head = `{"metadata":{"protocol":"%s","apiVersion":"2020-01-01","endpointPrefix":"loop",
			"targetPrefix":"Loop","jsonVersion":"1.0","xmlNamespace":"http://loop.example/"},
			"operations":{"Op":{"name":"Op","input":{"shape":"%s"},"output":{"shape":"%s"}}},
			"shapes":{"S":{"type":"string"},"Flat":{"type":"structure","members":{"S":{"shape":"S"}}},
			"In":{"type":"structure","members":{"Tree":{"shape":"Node"}}},`
		direct  = `"Node":{"type":"structure","members":{"Label":{"shape":"S"},"Child":{"shape":"Node"}}}}}`
		viaList = `"Node":{"type":"structure","members":{"Kids":{"shape":"Nodes"}}},
			"Nodes":{"type":"list","member":{"shape":"Node"}}}}`
		viaMap = `"Node":{"type":"structure","members":{"Kids":{"shape":"Wrap"}}},
			"Wrap":{"type":"structure","members":{"ByName":{"shape":"NodeMap"}}},
			"NodeMap":{"type":"map","key":{"shape":"S"},"value":{"shape":"Node"}}}}`
	)

	const (
		inRoot, flatRoot = "In", "Flat"
	)

	tests := []struct {
		name, protocol, input, output, shapes, wantErr string
	}{
		{"a struct that holds itself", protoQuery, inRoot, flatRoot, direct, "and Node holds itself below"},
		{"through a list", protoQuery, inRoot, flatRoot, viaList, "and Node holds itself below"},
		{"through a map and another struct", protoQuery, inRoot, flatRoot, viaMap, "and Node, Wrap holds itself below"},
		{"in an output only", protoQuery, flatRoot, inRoot, direct, ""},
		{"in a json service", protoJSON, inRoot, flatRoot, direct, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			model := fmt.Sprintf(head, tt.protocol, tt.input, tt.output) + tt.shapes

			svc, err := spec.ParseBytes([]byte(model))
			require.NoError(t, err)

			g, err := newModel("loop", serviceOptions{}, svc, nil)
			require.NoError(t, err)

			_, err = g.files()
			if tt.wantErr == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorContains(t, err, "the query decoder has no depth bound, "+tt.wantErr)
		})
	}
}
