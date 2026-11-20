package awsjson_test

import (
	"net/http"
	"runtime"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// maxNesting is how deeply arrays and objects may nest in a json body:
// encoding/json/v2's own limit, which every json, json 1.1 and rest-json body
// meets through Decode. docs/compat.md states it.
const maxNesting = 10000

type documentInput struct {
	Doc any `json:"Doc,omitzero"`
}

// arrays is a body whose Doc member nests arrays until the whole body is depth
// levels deep, counting the body's own object.
func arrays(depth int) string {
	return `{"Doc":` + strings.Repeat("[", depth-1) + strings.Repeat("]", depth-1) + "}"
}

// Test_Decode_NestingBound: a body nests up to maxNesting levels and decodes,
// and one level more is a 400 SerializationException, through both the strict
// and the lenient pass.
func Test_Decode_NestingBound(t *testing.T) {
	t.Parallel()

	_, failure := awsjson.Decode[documentInput]([]byte(arrays(maxNesting)))
	require.Nil(t, failure)

	_, failure = awsjson.Decode[documentInput]([]byte(arrays(maxNesting + 1)))
	require.NotNil(t, failure)
	assert.Equal(t, "SerializationException", failure.Code)
	assert.Equal(t, http.StatusBadRequest, failure.StatusCode)

	// A value the lenient pass alone takes still meets the bound there.
	lenient := `{"Flag":"true","Doc":` + strings.Repeat("[", maxNesting) + strings.Repeat("]", maxNesting) + "}"
	_, failure = awsjson.Decode[struct {
		Flag *bool `json:"Flag"`
		Doc  any   `json:"Doc"`
	}]([]byte(lenient))
	require.NotNil(t, failure)
	assert.Equal(t, "SerializationException", failure.Code)
}

// Test_Decode_NestedToTheBodyCap: a body of the 16 MiB cap that nests as
// deeply as json allows is refused, and what decoding allocates stays a small
// multiple of the body.
func Test_Decode_NestedToTheBodyCap(t *testing.T) {
	t.Parallel()

	size := int(protocol.DefaultMaxBody)
	for name, body := range map[string]string{
		"arrays":  strings.Repeat("[", size/2) + strings.Repeat("]", size/2),
		"objects": strings.Repeat(`{"a":`, size/6) + "1" + strings.Repeat("}", size/6),
		"member":  arrays(size / 2),
	} {
		var before, after runtime.MemStats

		runtime.ReadMemStats(&before)
		got, failure := awsjson.Decode[documentInput]([]byte(body))
		runtime.ReadMemStats(&after)

		require.Nil(t, got, name)
		require.NotNil(t, failure, name)
		assert.Equal(t, "SerializationException", failure.Code, name)
		assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(16*size), name)
	}
}
