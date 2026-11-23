package awsjson

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stamped is shaped like a generated struct with a timestamp in another
// format: its MarshalJSONTo and UnmarshalJSONFrom go through MarshalTo and
// UnmarshalFrom, as emitTimeMarshalers writes them.
type stamped struct {
	Count *int32     `json:"Count,omitzero"`
	Ratio *float64   `json:"Ratio,omitzero"`
	When  *ISO8601   `json:"When,omitzero"`
	Inner *strictAll `json:"Inner,omitzero"`
	Tags  []string   `json:"Tags,omitzero"`
}

func (s *stamped) MarshalJSONTo(enc *jsontext.Encoder) error {
	type plain stamped

	return MarshalTo(enc, (*plain)(s))
}

func (s *stamped) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	type plain stamped

	return UnmarshalFrom(dec, (*plain)(s))
}

// strictAll has each scalar a lenient reader or writer exists for, in every
// place a generated type holds one.
type strictAll struct {
	I     *int32             `json:"I,omitzero"`
	L     *int64             `json:"L,omitzero"`
	B     *bool              `json:"B,omitzero"`
	D     *float64           `json:"D,omitzero"`
	X     []byte             `json:"X,omitzero"`
	S     *string            `json:"S,omitzero"`
	Xs    [][]byte           `json:"Xs,omitzero"`
	ByKey map[string]float64 `json:"ByKey,omitzero"`
	Ints  []int32            `json:"Ints,omitzero"`
	Names map[string]string  `json:"Names,omitzero"`
	Stamp *stamped           `json:"Stamp,omitzero"`
	Next  *strictAll         `json:"Next,omitzero"`
}

// strictSeeds hold bodies the strict pass takes and bodies only the lenient
// pass takes.
var strictSeeds = []string{
	`{"I":5,"L":-9007199254740993,"B":true,"D":1.5,"X":"aGk=","S":"s"}`,
	`{"I":-0,"L":0,"D":-0,"X":"","Xs":["","QR=="],"ByKey":{"a":1e300}}`,
	`{"I":2147483647,"L":9223372036854775807,"B":false,"D":5e-324}`,
	`{"I":-2147483648,"L":-9223372036854775808,"I":null,"X":null}`,
	`{"Stamp":{"Count":7,"Ratio":0.25,"When":"2020-01-02T03:04:05Z","Inner":{"I":1}}}`,
	`{"Next":{"Next":{"I":3,"X":"aGVsbG8="}}}`,
	`{"I":"5","L":"1_0","B":"TRUE","D":"NaN","X":"aGk"}`,
	`{"I":5.7,"L":1e3,"B":"false","D":"-Infinity","X":"a\nGk="}`,
	`{"I":3000000000,"L":99999999999999999999,"ByKey":{"a":"Infinity"}}`,
	`{"Stamp":{"Count":"7","Ratio":"NaN","Inner":{"I":true}}}`,
	`{"Next":{"I":"x"}}`,
	`{"I":[5]}`,
	`[`,
	// A null in a list or a map, where the strict pass takes the body, where only the
	// lenient pass does, and where a null sits beside a value the lenient pass reads.
	`{"Xs":[null,"QQ==",null],"Ints":[null,1,null],"Names":{"a":null,"b":"x"},"Stamp":{"Tags":["a",null]}}`,
	`{"Ints":[null,"5",5.7,null],"Names":{"a":"x","a":null},"Xs":[null,"a\nGk"]}`,
	`{"Ints":[null],"Names":{"a":null},"Stamp":{"Count":"7","Tags":[null,"t"]}}`,
	`{"Ints":[1,null,{}]}`,
	`{"Names":{"a":null,"b":[1]}}`,
	// A member that repeats: v2 adds a repeated map to the one it set and replaces
	// a repeated list, with a null in the body, with the text null in a string
	// only, and with a lenient scalar beside it.
	`{"Names":{"a":"x"},"Names":{"b":"y"}}`,
	`{"Names":{"a":"x"},"Names":{"b":"y"},"S":"null"}`,
	`{"Names":{"a":"x"},"Names":{"b":"y"},"I":"5"}`,
	`{"Names":{"a":"x","c":"z"},"Names":{"a":null,"b":"y"}}`,
	`{"Names":{"a":"x"},"Names":null,"Names":{"b":"y"}}`,
	`{"Ints":[1],"Ints":[2],"S":"null"}`,
	`{"Ints":[1,null],"Ints":[null,2]}`,
	`{"Ints":[1],"Ints":[null,"5"]}`,
	// The text null in a string, and a null beside it.
	`{"S":"null","Ints":[1,2]}`,
	`{"S":"\"null\"","Ints":[null,2]}`,
	`{"S":"a\\","Names":{"a":null}}`,
}

// lenientOnly decodes data the way every decode ran before strict came first.
func lenientOnly(data []byte) (*strictAll, error) {
	v := new(strictAll)

	err := json.Unmarshal(data, v, lenient)
	if err != nil {
		return nil, err //nolint:wrapcheck // compared, not returned
	}

	return v, nil
}

// checkStrictAgrees is the property strict-first rests on: a body the strict
// pass takes decodes as the lenient pass alone decodes it, and Decode and
// Unmarshal give what the lenient pass alone gives for every body.
func checkStrictAgrees(t *testing.T, data []byte) {
	t.Helper()

	if len(bytes.TrimSpace(data)) == 0 {
		got, usedLenient, failure := DecodeLenient[strictAll](data)
		require.Nil(t, failure)
		require.False(t, usedLenient)
		same(t, new(strictAll), got, data)

		return
	}

	want, wantErr := lenientOnly(data)

	strictV := new(strictAll)
	if json.Unmarshal(data, strictV, strictFor(data)) == nil {
		require.NoError(t, wantErr, "%s: strict took a body lenient refuses", data)
		same(t, want, strictV, data)
	}

	got, usedLenient, failure := DecodeLenient[strictAll](data)
	if wantErr != nil {
		require.NotNil(t, failure, "%s", data)
		require.Nil(t, got)

		return
	}

	require.Nil(t, failure, "%s", data)
	same(t, want, got, data)
	require.Equal(t, json.Unmarshal(data, new(strictAll), strictFor(data)) != nil, usedLenient, "%s", data)

	into := new(strictAll)
	require.NoError(t, Unmarshal(data, into))
	same(t, want, into, data)
}

// same compares two decoded values by their json, so that NaN equals NaN,
// while nil and empty slices, and -0 and 0, still differ. Map keys are written
// in order, so a map of two entries has one json.
func same(t *testing.T, want, got *strictAll, data []byte) {
	t.Helper()

	w, err := json.Marshal(want, lenient, json.Deterministic(true))
	require.NoError(t, err)

	g, err := json.Marshal(got, lenient, json.Deterministic(true))
	require.NoError(t, err)
	require.Equal(t, string(w), string(g), "%s", data)
}

func Test_strict_first_decodes_as_lenient_alone(t *testing.T) {
	t.Parallel()

	for _, seed := range strictSeeds {
		checkStrictAgrees(t, []byte(seed))
	}
}

func Test_a_lenient_value_inside_a_timestamp_struct_is_read(t *testing.T) {
	t.Parallel()

	got, failure := Decode[strictAll]([]byte(`{"Stamp":{"Count":"7","Ratio":"NaN","Inner":{"I":true}}}`))
	require.Nil(t, failure)
	assert.Equal(t, int32(7), *got.Stamp.Count)
	assert.True(t, math.IsNaN(*got.Stamp.Ratio))
	assert.Equal(t, int32(1), *got.Stamp.Inner.I)
}

func Test_strict_first_encodes_as_lenient_alone(t *testing.T) {
	t.Parallel()

	for _, f := range []float64{0, -1.5, 1e300, 5e-324, math.NaN(), math.Inf(1), math.Inf(-1)} {
		v := &strictAll{D: &f, ByKey: map[string]float64{"k": f}, Stamp: &stamped{Ratio: &f}}

		want, err := json.Marshal(v, lenient)
		require.NoError(t, err)

		got, err := Marshal(v)
		require.NoError(t, err)
		assert.Equal(t, string(want), string(got), "%v", f)

		encoded, failure := Encode(v)
		require.Nil(t, failure)
		assert.Equal(t, string(want), string(encoded), "%v", f)
	}

	nan := math.NaN()
	got, err := Marshal(&strictAll{D: &nan})
	require.NoError(t, err)
	assert.JSONEq(t, `{"D":"NaN"}`, string(got))
}

func FuzzStrictAgreesWithLenient(f *testing.F) {
	for _, seed := range strictSeeds {
		f.Add([]byte(seed))
	}

	f.Fuzz(checkStrictAgrees)
}
