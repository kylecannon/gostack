package s3

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestConditionText: a failed condition is reported as Python's str() of the
// parsed policy condition, with ' made ". Objects keep their document order and
// floats are Python's repr.
func TestConditionText(t *testing.T) {
	t.Parallel()

	const million = `["eq", "$key", 1000000.0]`

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"a million as a float", `["eq", "$key", 1000000.0]`, million},
		{"a million by exponent", `["eq", "$key", 1e6]`, million},
		{"a million with a fraction digit", `["eq", "$key", 1000000.50]`, `["eq", "$key", 1000000.5]`},
		{"the last positional float", `["eq", "$key", 1e15]`, `["eq", "$key", 1000000000000000.0]`},
		{"the first exponent float", `["eq", "$key", 1e16]`, `["eq", "$key", 1e+16]`},
		{"a long mantissa in exponent form", `["eq", "$key", 1.5e22]`, `["eq", "$key", 1.5e+22]`},
		{"a fraction", `["eq", "$key", 0.5]`, `["eq", "$key", 0.5]`},
		{"the last positional small float", `["eq", "$key", 0.0001]`, `["eq", "$key", 0.0001]`},
		{"the first small exponent float", `["eq", "$key", 0.00001]`, `["eq", "$key", 1e-05]`},
		{"a negative float", `["eq", "$key", -2.50]`, `["eq", "$key", -2.5]`},
		{"an integer stays one", `["eq", "$key", 1000000]`, `["eq", "$key", 1000000]`},
		{"a float beyond range", `["eq", "$key", 1e999]`, `["eq", "$key", inf]`},
		{"words", `["eq", "$key", true, false, null]`, `["eq", "$key", True, False, None]`},
		{
			"an object in document order", `["eq", "$key", {"zeta": 1, "alpha": 2, "mid": {"y": 1.0, "b": 2}}]`,
			`["eq", "$key", {"zeta": 1, "alpha": 2, "mid": {"y": 1.0, "b": 2}}]`,
		},
		{
			"a repeated name keeps its first place and the last value", `{"b": 1, "a": 2, "b": 3}`,
			`{"b": 3, "a": 2}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, conditionText(json.RawMessage(test.raw)))
		})
	}
}

// TestConditionTextIsStable: Go's map order is random, Python's dict order is
// the document's, so 50 renderings of one object must be the same text.
func TestConditionTextIsStable(t *testing.T) {
	t.Parallel()

	const raw = `{"k9": 1, "k1": 2, "k5": 3, "k3": 4, "k7": 5, "k2": 6, "k8": 7, "k4": 8, "k6": 9}`

	want := conditionText(json.RawMessage(raw))
	assert.Equal(t, `{"k9": 1, "k1": 2, "k5": 3, "k3": 4, "k7": 5, "k2": 6, "k8": 7, "k4": 8, "k6": 9}`, want)

	for range 50 {
		assert.Equal(t, want, conditionText(json.RawMessage(raw)))
	}
}
