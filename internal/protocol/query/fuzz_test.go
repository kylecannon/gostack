package query_test

import (
	"net/url"
	"testing"

	"github.com/kylecannon/gostack/codegen/spec"
	"github.com/kylecannon/gostack/internal/protocol/query"
	"github.com/stretchr/testify/require"
)

// fuzzModel has one of each thing the decoder reads: scalars of every type, a
// flattened and a plain list, a flattened map of structures, and a structure
// that holds itself.
const fuzzModel = `{"shapes":{
  "Input":{"type":"structure","members":{
    "Str":{"shape":"S"},"Int":{"shape":"I"},"Long":{"shape":"L"},"Dbl":{"shape":"D"},"Flag":{"shape":"B"},
    "Data":{"shape":"Blob"},"When":{"shape":"T"},"Epoch":{"shape":"TU"},
    "Names":{"shape":"Strs","flattened":true,"locationName":"Name"},
    "Items":{"shape":"Items"},
    "Attrs":{"shape":"Attrs","flattened":true,"locationName":"Attr"},
    "Tree":{"shape":"Node"}}},
  "Node":{"type":"structure","members":{"Label":{"shape":"S"},"Child":{"shape":"Node"},"Kids":{"shape":"Nodes"}}},
  "Nodes":{"type":"list","member":{"shape":"Node","locationName":"kid"}},
  "Item":{"type":"structure","members":{"Key":{"shape":"S"},"Vals":{"shape":"Strs"}}},
  "Items":{"type":"list","member":{"shape":"Item"}},
  "Strs":{"type":"list","member":{"shape":"S"}},
  "Attrs":{"type":"map","key":{"shape":"S","locationName":"N"},"value":{"shape":"Attr"}},
  "Attr":{"type":"structure","members":{"Value":{"shape":"S"},"Num":{"shape":"L"}}},
  "S":{"type":"string"},"I":{"type":"integer"},"L":{"type":"long"},"D":{"type":"double"},"B":{"type":"boolean"},
  "Blob":{"type":"blob"},"T":{"type":"timestamp"},"TU":{"type":"timestamp","timestampFormat":"unixTimestamp"}}}`

func FuzzQueryDecode(f *testing.F) {
	svc, err := spec.ParseBytes([]byte(fuzzModel))
	require.NoError(f, err)

	in := &interp{shapes: svc.Shapes}

	f.Add("Str=a&Int=1&Long=-9&Dbl=1.5&Flag=true&Data=aGk%3D&When=2000-01-02T20%3A34%3A56Z&Epoch=946845296")
	f.Add("Name.1=a&Name.2=b&Name.4=skipped&Items.member.1.Key=k&Items.member.1.Vals.member.1=v")
	f.Add("Attr.1.N=a&Attr.1.value.Value=x&Attr.1.value.Num=3&Attr.2.N=b&Attr.2.value.Value=y")
	f.Add("Tree.Label=root&Tree.Child.Label=c&Tree.Child.Child.Label=cc&Tree.kid.member.1.Label=k")
	f.Add("Int=99999999999&Flag=maybe&Data=%25&When=never&Dbl=NaN&Items.member.10.Key=far")

	f.Fuzz(func(t *testing.T, raw string) {
		vals, _ := url.ParseQuery(raw)

		d := query.NewDecoder(vals)
		got := in.decodeStruct(d, svc.Shapes["Input"], "")

		failure := d.Err()
		if failure != nil {
			require.Equal(t, 400, failure.StatusCode)
			require.Equal(t, "InvalidParameterValue", failure.Code)

			return
		}

		// A request that decoded cleanly must decode to the same thing again.
		again := in.decodeStruct(query.NewDecoder(vals), svc.Shapes["Input"], "")
		require.Equal(t, canonical(got, keepEmpty), canonical(again, keepEmpty))
	})
}
