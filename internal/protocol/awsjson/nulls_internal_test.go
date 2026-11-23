package awsjson

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// elemSamples is a json value of each element type a dropper covers, written
// in the form the strict pass takes.
var elemSamples = map[reflect.Type]string{
	reflect.TypeFor[string]():  `"a"`,
	reflect.TypeFor[int32]():   `7`,
	reflect.TypeFor[int64]():   `-9007199254740993`,
	reflect.TypeFor[float64](): `1.5`,
	reflect.TypeFor[bool]():    `true`,
	reflect.TypeFor[[]byte]():  `"YQ=="`,
}

// decodeAs decodes body into a new value of type t with Unmarshal.
func decodeAs(t *testing.T, typ reflect.Type, body string) reflect.Value {
	t.Helper()

	v := reflect.New(typ)
	require.NoError(t, Unmarshal([]byte(body), v.Interface()), "%T %s", v.Interface(), body)

	return v.Elem()
}

func Test_a_null_in_a_list_or_map_of_scalars_is_dropped(t *testing.T) {
	t.Parallel()

	require.Len(t, droppers, 2*len(elemSamples), "a list and a map for each scalar a generated type holds")

	for _, d := range droppers {
		t.Run(d.typ.String(), func(t *testing.T) {
			t.Parallel()

			sample := elemSamples[d.typ.Elem()]
			require.NotEmpty(t, sample)

			one := decodeAs(t, d.typ.Elem(), sample) // the element a client sent, as it decodes alone

			if d.typ.Kind() == reflect.Slice {
				got := decodeAs(t, d.typ, `[null,`+sample+`,null]`)
				require.Equal(t, 1, got.Len(), "a list keeps what was sent and nothing else")
				assert.Equal(t, one.Interface(), got.Index(0).Interface())

				for _, empty := range []string{`[null]`, `[null,null]`, `[]`} {
					got = decodeAs(t, d.typ, empty)
					assert.False(t, got.IsNil(), "%s is an empty list, as [] is", empty)
					assert.Zero(t, got.Len(), empty)
				}

				assert.True(t, decodeAs(t, d.typ, `null`).IsNil(), "a null list is absent")

				return
			}

			got := decodeAs(t, d.typ, `{"a":null,"b":`+sample+`,"c":null}`)
			require.Equal(t, 1, got.Len(), "a map keeps what was sent and nothing else")
			assert.Equal(t, one.Interface(), got.MapIndex(reflect.ValueOf("b")).Interface())

			got = decodeAs(t, d.typ, `{"a":`+sample+`,"a":null}`)
			assert.Zero(t, got.Len(), "the last value of a repeated name wins, and a null removes it")

			got = decodeAs(t, d.typ, `{"a":null,"a":`+sample+`}`)
			assert.Equal(t, 1, got.Len())

			got = decodeAs(t, d.typ, `{"a":null}`)
			assert.False(t, got.IsNil(), "an object of nothing is an empty map, as {} is")
			assert.Zero(t, got.Len())
			assert.True(t, decodeAs(t, d.typ, `null`).IsNil(), "a null map is absent")
		})
	}
}

// A zero value the client sent stays.
func Test_a_zero_value_in_a_list_or_map_is_kept(t *testing.T) {
	t.Parallel()

	type body struct {
		Strs  []string          `json:"Strs"`
		Ints  []int32           `json:"Ints"`
		Bools []bool            `json:"Bools"`
		Blobs [][]byte          `json:"Blobs"`
		ByKey map[string]string `json:"ByKey"`
	}

	got, usedLenient, failure := DecodeLenient[body](
		[]byte(`{"Strs":["",null,""],"Ints":[0,null],"Bools":[false],"Blobs":["",null],"ByKey":{"k":"","n":null}}`))
	require.Nil(t, failure)
	assert.False(t, usedLenient, "a body the strict pass takes is not read again")
	assert.Equal(t, []string{"", ""}, got.Strs)
	assert.Equal(t, []int32{0}, got.Ints)
	assert.Equal(t, []bool{false}, got.Bools)
	assert.Equal(t, [][]byte{{}}, got.Blobs)
	assert.Equal(t, map[string]string{"k": ""}, got.ByKey)
}

// The lenient pass reads the same list the strict pass would, with the nulls
// left out, so a body that needs it loses no null on the way.
func Test_a_null_is_dropped_in_the_lenient_pass_too(t *testing.T) {
	t.Parallel()

	type body struct {
		Ints  []int32           `json:"Ints"`
		ByKey map[string]int64  `json:"ByKey"`
		Blobs [][]byte          `json:"Blobs"`
		Names map[string]string `json:"Names"`
	}

	got, usedLenient, failure := DecodeLenient[body]([]byte(
		`{"Ints":[null,"5",null,5.7],"ByKey":{"a":null,"b":"1_0"},"Blobs":[null,"YQ=="],"Names":{"x":null}}`))
	require.Nil(t, failure)
	assert.True(t, usedLenient)
	assert.Equal(t, []int32{5, 5}, got.Ints)
	assert.Equal(t, map[string]int64{"b": 10}, got.ByKey)
	assert.Equal(t, [][]byte{[]byte("a")}, got.Blobs)
	assert.Empty(t, got.Names)
}

// A body with a null in a collection the decoders do not cover decodes as it
// always did.
func Test_a_null_elsewhere_is_left_as_it_was(t *testing.T) {
	t.Parallel()

	type leaf struct {
		Name *string `json:"Name"`
	}

	type body struct {
		Leaves []leaf              `json:"Leaves"`
		Ptrs   []*string           `json:"Ptrs"`
		Matrix [][]string          `json:"Matrix"`
		ByName map[string][]string `json:"ByName"`
		Name   *string             `json:"Name"`
	}

	got, _, failure := DecodeLenient[body]([]byte(
		`{"Leaves":[null],"Ptrs":[null,"a"],"Matrix":[null,["x"]],"ByName":{"k":null},"Name":null}`))
	require.Nil(t, failure)

	want := new(body)
	require.NoError(t, json.Unmarshal(
		[]byte(`{"Leaves":[null],"Ptrs":[null,"a"],"Matrix":[null,["x"]],"ByName":{"k":null},"Name":null}`), want, strict))
	assert.Equal(t, want, got)
	assert.Len(t, got.Leaves, 1, "a null structure is still one empty structure")
	assert.Len(t, got.Ptrs, 2, "a null pointer element is still nil")
	assert.Nil(t, got.Ptrs[0])
}

func Test_a_malformed_list_or_map_is_still_refused(t *testing.T) {
	t.Parallel()

	type body struct {
		Strs  []string          `json:"Strs"`
		ByKey map[string]string `json:"ByKey"`
	}

	for _, in := range []string{
		`{"Strs":["a",null`,
		`{"Strs":["a",null,]}`,
		`{"Strs":[1]}`,
		`{"Strs":"a"}`,
		`{"Strs":{"a":null}}`,
		`{"ByKey":{"a":null`,
		`{"ByKey":{"a":null,}}`,
		`{"ByKey":{"a":1}}`,
		`{"ByKey":["a",null]}`,
		`{"ByKey":{null:null}}`,
	} {
		got, failure := Decode[body]([]byte(in))
		require.NotNil(t, failure, in)
		assert.Nil(t, got, in)
		assert.Equal(t, "SerializationException", failure.Code, in)
	}
}

func Test_DropsNulls(t *testing.T) {
	t.Parallel()

	assert.True(t, DropsNulls(reflect.TypeFor[[]string]()))
	assert.True(t, DropsNulls(reflect.TypeFor[[][]byte]()))
	assert.True(t, DropsNulls(reflect.TypeFor[map[string]bool]()))
	assert.False(t, DropsNulls(reflect.TypeFor[[]*string]()), "a pointer element already keeps a null")
	assert.False(t, DropsNulls(reflect.TypeFor[[]struct{}]()))
	assert.False(t, DropsNulls(reflect.TypeFor[map[string][]string]()))
	assert.False(t, DropsNulls(reflect.TypeFor[string]()))
}

func Test_strictFor(t *testing.T) {
	t.Parallel()

	// Only a body with a null pays for the droppers, which look up every value.
	assert.Equal(t, strict, strictFor([]byte(`{"a":["x"]}`)))
	assert.NotEqual(t, strict, strictFor([]byte(`{"a":["x",null]}`)))

	// The text null inside a string is no null.
	assert.Equal(t, strict, strictFor([]byte(`{"a":"null"}`)))
	assert.Equal(t, strict, strictFor([]byte(`{"MessageBody":"{\"a\":null,\"b\":[null,2]}"}`)))
}

// nullTokenCases are bodies and whether hasNullToken counts a null in them: any
// body with a json null outside a string, and any body whose null text it cannot
// place as a string's by what surrounds it.
var nullTokenCases = []struct {
	body string
	want bool
}{
	{``, false},
	{`{}`, false},
	{`{"a":"x"}`, false},
	{`{"a":null}`, true},
	{`[null]`, true},
	{`{"a":["x" ,null]}`, true},
	{`null`, true},
	{`{"a":"null"}`, false},
	{`{"null":1}`, false},
	{`{"null":null}`, true},
	{`{"a":"x\"null"}`, false},
	{`{"a":"x\\","b":null}`, true},
	{`{"a":"x\\\"null"}`, false},
	{`{"a":"\\\\","b":"null"}`, false},
	{`{"MessageBody":"{\"a\":null,\"b\":[null,2]}"}`, false},
	{`{"MessageBody":"{\"a\":null}","Attrs":{"k":null}}`, true},
	{`{"a":"unterminated null`, false},
	{`{"a":"x","b":nul`, false},
	{`{"n":"nnull","m":nnull}`, false}, // not json, and left to the decoder to refuse
	{`{"S":"a, null, b"}`, true},       // text that looks like a list, and cannot be told from one
	{`{"S":":null,"}`, true},
	{`["null","null","null","null","null","null","null","null"]`, false},
	{`["null","null","null","null","null","null","null","null","null"]`, true}, // past the eighth it stops
}

func checkIndexNull(t *testing.T, b []byte) {
	t.Helper()

	assert.Equal(t, bytes.Index(b, nullToken), indexNull(b), "%q", b)
}

// indexNull finds the first null wherever it sits in a word, across two words,
// and in the tail past the last whole one.
func Test_indexNull_is_bytes_Index(t *testing.T) {
	t.Parallel()

	for pad := range 20 {
		for _, tail := range []string{"", "x", "xxxxxxxxx"} {
			prefix := strings.Repeat("u", pad)
			checkIndexNull(t, []byte(prefix+"null"+tail))
			checkIndexNull(t, []byte(prefix+"nul"+tail))
			checkIndexNull(t, []byte(prefix+"nuull"+tail))
			checkIndexNull(t, []byte(prefix+"nunull"+tail))
			checkIndexNull(t, []byte(prefix+"null-null"+tail))
			checkIndexNull(t, []byte(prefix+"nu"+tail))
			checkIndexNull(t, []byte(prefix+"ull"+tail))
		}
	}

	for _, tc := range nullTokenCases {
		checkIndexNull(t, []byte(tc.body))
	}

	checkIndexNull(t, nil)
	checkIndexNull(t, []byte("u"))
}

func FuzzIndexNull(f *testing.F) {
	for _, tc := range nullTokenCases {
		f.Add([]byte(tc.body))
	}

	f.Add([]byte("nnnnnnnnullllllll"))

	f.Fuzz(checkIndexNull)
}

func Test_hasNullToken(t *testing.T) {
	t.Parallel()

	for _, tc := range nullTokenCases {
		assert.Equal(t, tc.want, hasNullToken([]byte(tc.body)), tc.body)
	}
}

// hasNullTokenByTokens reports whether valid json has a null token, from the
// decoder's own tokens.
func hasNullTokenByTokens(body []byte) bool {
	dec := jsontext.NewDecoder(bytes.NewReader(body))

	for {
		tok, err := dec.ReadToken()
		if err != nil {
			return false
		}

		if tok.Kind() == 'n' {
			return true
		}
	}
}

func checkHasNullTokenAgrees(t *testing.T, body []byte) {
	t.Helper()

	if !jsontext.Value(body).IsValid() {
		return
	}

	if hasNullTokenByTokens(body) {
		assert.True(t, hasNullToken(body), "a null token is never missed: %s", body)
	}
}

func Test_hasNullToken_never_misses_a_null_token(t *testing.T) {
	t.Parallel()

	for _, tc := range nullTokenCases {
		checkHasNullTokenAgrees(t, []byte(tc.body))
	}

	for _, seed := range strictSeeds {
		checkHasNullTokenAgrees(t, []byte(seed))
	}
}

func FuzzHasNullToken(f *testing.F) {
	for _, tc := range nullTokenCases {
		f.Add([]byte(tc.body))
	}

	f.Add([]byte(`{"a":"\\\\\\\"null","b":[1,null]}`))

	f.Fuzz(checkHasNullTokenAgrees)
}

// A repeated member merges into the map it set, as v2's default decoding does,
// whatever else the body holds.
func Test_a_repeated_map_member_adds_to_the_map(t *testing.T) {
	t.Parallel()

	type body struct {
		Names map[string]string `json:"Names"`
		Ints  []int32           `json:"Ints"`
		S     *string           `json:"S"`
		I     *int32            `json:"I"`
	}

	for _, tc := range []struct {
		in    string
		names map[string]string
		ints  []int32
		v2    bool // v2's own decoding gives the same: the body holds no null
	}{
		{`{"Names":{"a":"x"},"Names":{"b":"y"}}`, map[string]string{"a": "x", "b": "y"}, nil, true},
		{`{"Names":{"a":"x"},"Names":{"b":"y"},"S":"null"}`, map[string]string{"a": "x", "b": "y"}, nil, true},
		{`{"Names":{"a":"x"},"Names":{"b":"y"},"I":"5"}`, map[string]string{"a": "x", "b": "y"}, nil, false},
		{`{"Names":{"a":"x"},"Names":{"b":"y"},"I":"5","S":"null"}`, map[string]string{"a": "x", "b": "y"}, nil, false},
		{`{"Names":{"a":"x","c":"z"},"Names":{"a":null,"b":"y"}}`, map[string]string{"b": "y", "c": "z"}, nil, false},
		{`{"Names":{"a":"x"},"Names":{}}`, map[string]string{"a": "x"}, nil, true},
		{`{"Ints":[1],"Ints":[2],"S":"null"}`, nil, []int32{2}, true},
		{`{"Ints":[1],"Ints":[null,2],"I":"5"}`, nil, []int32{2}, false},
	} {
		got, _, failure := DecodeLenient[body]([]byte(tc.in))
		require.Nil(t, failure, tc.in)
		assert.Equal(t, tc.names, got.Names, tc.in)
		assert.Equal(t, tc.ints, got.Ints, tc.in)

		if tc.v2 {
			plain := new(body)
			require.NoError(t, json.Unmarshal([]byte(tc.in), plain, strict), tc.in)
			assert.Equal(t, plain.Names, got.Names, tc.in)
			assert.Equal(t, plain.Ints, got.Ints, tc.in)
		}
	}
}

// KeepNulls reads a null in a list or map as v2 does, as the zero value, on the
// strict pass and on the lenient one, and drops nothing.
func Test_KeepNulls_reads_a_null_as_the_zero_value(t *testing.T) {
	t.Parallel()

	type body struct {
		Strs  []string          `json:"Strs"`
		Ints  []int32           `json:"Ints"`
		ByKey map[string]string `json:"ByKey"`
		I     *int32            `json:"I"`
	}

	for _, tc := range []struct {
		in          string
		usedLenient bool
	}{
		{`{"Strs":[null,"a"],"Ints":[null],"ByKey":{"k":null}}`, false},
		{`{"Strs":[null,"a"],"Ints":[null],"ByKey":{"k":null},"I":"5"}`, true},
	} {
		got, usedLenient, failure := DecodeLenient[body]([]byte(tc.in), KeepNulls)
		require.Nil(t, failure, tc.in)
		assert.Equal(t, tc.usedLenient, usedLenient, tc.in)
		assert.Equal(t, []string{"", "a"}, got.Strs, tc.in)
		assert.Equal(t, []int32{0}, got.Ints, tc.in)
		assert.Equal(t, map[string]string{"k": ""}, got.ByKey, tc.in)

		dropped, _, failure := DecodeLenient[body]([]byte(tc.in))
		require.Nil(t, failure, tc.in)
		assert.Equal(t, []string{"a"}, dropped.Strs, "the same body without the option drops the null")
	}

	got, failure := Decode[body]([]byte(`{"Strs":[null]}`), KeepNulls)
	require.Nil(t, failure)
	assert.Equal(t, []string{""}, got.Strs)

	within, _, failure := DecodeWithin[body]([]byte(`{"Strs":[null]}`), 1<<20, nil, KeepNulls)
	require.Nil(t, failure)
	assert.Equal(t, []string{""}, within.Strs)
}
