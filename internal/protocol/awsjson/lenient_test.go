package awsjson_test

import (
	"math"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol/awsjson"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type scalars struct {
	I *int32   `json:"I"`
	L *int64   `json:"L"`
	B *bool    `json:"B"`
	D *float64 `json:"D"`
	X []byte   `json:"X"`
	N []int32  `json:"N"`
}

const hello = "hello"

// Test_Decode_reads_what_LocalStack_reads is SQ-63 for the json protocol.
// LocalStack 8b9a79f answered each accepted value with a 200 and each refused
// one with a 500 in ReceiveMessage (verification/SQ-145/ls-probe.out); a
// refused value is a 400 SerializationException here.
func Test_Decode_reads_what_LocalStack_reads(t *testing.T) {
	t.Parallel()

	integers := map[string]int32{
		`{"I":"5"}`:               5,
		`{"I":" 5"}`:              5,
		`{"I":"1_0"}`:             10,
		`{"I":"+5"}`:              5,
		`{"I":5.0}`:               5,
		`{"I":5.7}`:               5,
		`{"I":-0.5}`:              0,
		`{"I":1e1}`:               10,
		`{"I":true}`:              1,
		`{"I":false}`:             0,
		`{"I":3000000000}`:        math.MaxInt32,
		`{"I":"-3000000000"}`:     math.MinInt32,
		`{"I":99999999999999999}`: math.MaxInt32,
	}

	for body, want := range integers {
		got, err := awsjson.Decode[scalars]([]byte(body))

		require.Nil(t, err, body)
		assert.Equal(t, want, *got.I, body)
	}

	got, err := awsjson.Decode[scalars]([]byte(`{"L":99999999999999999999}`))
	require.Nil(t, err)
	assert.Equal(t, int64(math.MaxInt64), *got.L)

	got, err = awsjson.Decode[scalars]([]byte(`{"L":"9007199254740993"}`))
	require.Nil(t, err)
	assert.Equal(t, int64(9007199254740993), *got.L)

	got, err = awsjson.Decode[scalars]([]byte(`{"I":null,"L":null,"B":null,"X":null}`))
	require.Nil(t, err)
	assert.Nil(t, got.I, "a null integer is absent")
	assert.Nil(t, got.L)
	assert.Nil(t, got.B)
	assert.Nil(t, got.X)

	got, err = awsjson.Decode[scalars]([]byte(`{"N":["1"," 2",3.0,true]}`))
	require.Nil(t, err)
	assert.Equal(t, []int32{1, 2, 3, 1}, got.N)

	for body, want := range map[string]bool{`{"B":"TRUE"}`: true, `{"B":false}`: false, `{"B":"false"}`: false} {
		got, err = awsjson.Decode[scalars]([]byte(body))
		require.Nil(t, err, body)
		assert.Equal(t, want, *got.B, body)
	}

	got, err = awsjson.Decode[scalars]([]byte(`{"D":" 1_0.5"}`))
	require.Nil(t, err)
	assert.InDelta(t, 10.5, *got.D, 0)

	got, err = awsjson.Decode[scalars]([]byte(`{"D":"-Infinity"}`))
	require.Nil(t, err)
	assert.True(t, math.IsInf(*got.D, -1))

	for _, value := range []string{"aGVsbG8=", "aGVsbG8==", "aGVsbG8=aGVsbG8=", `aGVs\nbG8=-_$`} {
		got, err = awsjson.Decode[scalars]([]byte(`{"X":"` + value + `"}`))
		require.Nil(t, err, value)
		assert.Equal(t, hello, string(got.X), value)
	}
}

func Test_Decode_refuses_what_LocalStack_refuses(t *testing.T) {
	t.Parallel()

	refused := map[string]string{
		"word":               `{"I":"abc"}`,
		"string fraction":    `{"I":"5.0"}`,
		"list":               `{"I":[5]}`,
		"object":             `{"I":{}}`,
		"overflowing number": `{"I":1e400}`,
		"empty string":       `{"I":""}`,
		"integer boolean":    `{"B":1}`,
		"word boolean":       `{"B":"yes"}`,
		"unpadded blob":      `{"X":"aGVsbG8"}`,
		"dangling blob":      `{"X":"aGVsb"}`,
		"blob not a string":  `{"X":5}`,
		"double word":        `{"D":"abc"}`,
	}

	for name, body := range refused {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := awsjson.Decode[scalars]([]byte(body))

			require.NotNil(t, err)
			assert.Nil(t, got)
			assert.Equal(t, "SerializationException", err.Code)
			assert.Equal(t, 400, err.StatusCode)
		})
	}
}
