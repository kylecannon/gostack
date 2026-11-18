package query_test

import (
	"math"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol/query"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_Decoder_reads_what_LocalStack_reads is SQ-63: LocalStack reads integers
// with Python's int(), floats with float() and blobs with b64decode(), and
// takes what they take. Each value was sent to LocalStack 8b9a79f in
// ReceiveMessage (verification/SQ-145/ls-probe.out); the ones it refuses with a
// 500 stay a 400 here.
const helloText = "hello"

func Test_Decoder_reads_what_LocalStack_reads(t *testing.T) {
	t.Parallel()

	ints := []struct {
		raw  string
		want int32
	}{
		{"%205", 5},                    // " 5"
		{"5%20", 5},                    // "5 "
		{"1_0", 10},                    // underscore between digits
		{"%2B5", 5},                    // "+5"
		{"-0", 0},                      //
		{"007", 7},                     //
		{"%D9%A5", 5},                  // an Arabic-Indic digit
		{"3000000000", math.MaxInt32},  // past int32: the nearest bound
		{"-3000000000", math.MinInt32}, //
		{"99999999999999999999", math.MaxInt32},
	}

	for _, tt := range ints {
		d := query.NewDecoder(values(t, "N="+tt.raw))

		got := d.Int32("N")

		require.Nil(t, d.Err(), tt.raw)
		require.NotNil(t, got, tt.raw)
		assert.Equal(t, tt.want, *got, tt.raw)
	}

	for _, raw := range []string{"5.0", "0x10", "", "1e1", "1__0", "_1", "abc", "%E4%BA%94"} {
		d := query.NewDecoder(values(t, "N="+raw))

		d.Int32("N")

		require.NotNil(t, d.Err(), raw)
		assert.Equal(t, 400, d.Err().StatusCode, raw)
	}

	d := query.NewDecoder(values(t, "L=99999999999999999999"))
	assert.Equal(t, int64(math.MaxInt64), *d.Int64("L"))

	d = query.NewDecoder(values(t, "F=%201_0.5e1%20&G=%2BInf&H=-nan"))
	assert.InDelta(t, 105.0, *d.Float64("F"), 0)
	assert.True(t, math.IsInf(*d.Float64("G"), 1))
	assert.True(t, math.IsNaN(*d.Float64("H")))
	require.Nil(t, d.Err())

	blobs := map[string]string{
		"aGVsbG8%3D":           helloText,
		"aGVsbG8%3D%3D":        helloText, // extra padding
		"aGVsbG8%3DaGVsbG8%3D": helloText, // data after the padding is ignored
		"aGVs%0AbG8%3D":        helloText, // a newline is skipped
		"aGV%24sbG8%3D":        helloText, // so is any character outside the alphabet
		"":                     "",
		"%3D%3D%3D%3D":         "",
	}

	for raw, want := range blobs {
		d := query.NewDecoder(values(t, "B="+raw))

		got := d.Blob("B")

		require.Nil(t, d.Err(), raw)
		assert.Equal(t, want, string(got), raw)
	}

	for _, raw := range []string{"aGVsbG8", "aGVsb", "aGVs%C3%A4bG8%3D"} {
		d := query.NewDecoder(values(t, "B="+raw))

		d.Blob("B")

		require.NotNil(t, d.Err(), raw)
	}
}
