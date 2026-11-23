package awsjson

import (
	"encoding/json/v2"
	"reflect"
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
}
