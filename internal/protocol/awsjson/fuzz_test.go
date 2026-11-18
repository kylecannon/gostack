package awsjson_test

import (
	"net/http"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
	"github.com/stretchr/testify/require"
)

type fuzzLeaf struct {
	Name *string           `json:"Name,omitzero"`
	Tags map[string]string `json:"Tags,omitzero"`
}

// fuzzInput has one of each thing a generated type holds.
type fuzzInput struct {
	Str     *string               `json:"Str,omitzero"`
	Int     *int32                `json:"Int,omitzero"`
	Long    *int64                `json:"Long,omitzero"`
	Dbl     *float64              `json:"Dbl,omitzero"`
	Flag    *bool                 `json:"Flag,omitzero"`
	Data    []byte                `json:"Data,omitzero"`
	When    *protocol.Time        `json:"When,omitzero"`
	Names   []string              `json:"Names,omitzero"`
	Leaves  []fuzzLeaf            `json:"Leaves,omitzero"`
	ByName  map[string]fuzzLeaf   `json:"ByName,omitzero"`
	Nested  *fuzzInput            `json:"Nested,omitzero"`
	Matrix  [][]string            `json:"Matrix,omitzero"`
	Lookups map[string][]fuzzLeaf `json:"Lookups,omitzero"`
}

func FuzzAWSJSONDecode(f *testing.F) {
	f.Add([]byte(`{"Str":"a","Int":1,"Long":-9,"Dbl":1.5,"Flag":true,"Data":"aGk=","When":946845296.5}`))
	f.Add([]byte(`{"Names":["a","b"],"Leaves":[{"Name":"x","Tags":{"k":"v"}}],"ByName":{"n":{"Name":"y"}}}`))
	f.Add([]byte(`{"Nested":{"Nested":{"Str":"deep"}},"Matrix":[["a"],[]],"Lookups":{"k":[{"Name":"z"}]}}`))
	f.Add([]byte(`{"Int":99999999999,"Flag":"yes","Data":"%","When":"never","Names":[1,null],"str":"case"}`))
	f.Add([]byte(`[`))

	f.Fuzz(func(t *testing.T, body []byte) {
		got, failure := awsjson.Decode[fuzzInput](body)
		if failure != nil {
			require.Nil(t, got)
			require.Equal(t, "SerializationException", failure.Code)
			require.Equal(t, http.StatusBadRequest, failure.StatusCode)

			return
		}

		// Whatever decoded can be written back as json.
		_, failure = awsjson.Encode(got)
		require.Nil(t, failure)
	})
}
