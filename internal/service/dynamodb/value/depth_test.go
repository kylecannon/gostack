package value_test

import (
	"runtime/debug"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

// levelsOf is a string inside levels-1 maps.
func levelsOf(levels int) api.AttributeValue {
	s := "x"
	v := api.AttributeValue{S: &s}

	for i := 1; i < levels; i++ {
		v = api.AttributeValue{M: map[string]api.AttributeValue{"a": v}}
	}

	return v
}

// Test_depthBoundaries: an attribute of an item nests at most MaxDepth (32)
// levels, and a value outside an item, one in ExpressionAttributeValues, one
// more (SQ-280, probed on the LocalStack image: 33 levels pass a filter, 34
// do not, while a put or an updated item takes 32).
func Test_depthBoundaries(t *testing.T) {
	t.Parallel()

	for levels, want := range map[int][3]bool{ // value ok, item attribute ok, TooDeep
		31: {true, true, false}, 32: {true, true, false}, 33: {true, false, true}, 34: {false, false, true},
	} {
		v := levelsOf(levels)
		require.Equal(t, want[0], value.Validate(new(levelsOf(levels))) == nil, "Validate %d levels", levels)
		require.Equal(t, want[1], value.ValidateItem(value.Item{"d": v}) == nil, "ValidateItem %d levels", levels)
		require.Equal(t, want[2], value.TooDeep(&v), "TooDeep %d levels", levels)
	}

	empty := api.AttributeValue{M: value.Item{}}
	for range value.MaxDepth - 1 {
		empty = api.AttributeValue{L: []api.AttributeValue{empty}}
	}

	require.False(t, value.TooDeep(&empty), "an empty map at level 32")

	empty = api.AttributeValue{L: []api.AttributeValue{empty}}
	require.True(t, value.TooDeep(&empty), "an empty map at level 33")
}

// Test_Validate_deepBounded: a value nested 100,000 levels deep is refused
// within a 256 KiB stack, so the 32-level check stops descending at the limit
// and never walks the rest. A check that recursed through every level would
// pass the stack limit, which ends the test binary.
//
// It does not run in parallel: SetMaxStack applies to every goroutine, and
// Go starts the parallel tests only after the serial ones finish.
//
//nolint:paralleltest // SetMaxStack is process-wide; see above.
func Test_Validate_deepBounded(t *testing.T) {
	const levels = 100_000

	s := "x"
	v := api.AttributeValue{S: &s}

	for range levels {
		v = api.AttributeValue{M: map[string]api.AttributeValue{"a": v}}
	}

	item := value.Item{"d": v}

	const stack = 256 << 10

	defer debug.SetMaxStack(debug.SetMaxStack(stack))

	require.ErrorContains(t, value.Validate(&v), "Nesting Levels have exceeded supported limits")
	require.ErrorContains(t, value.ValidateItem(item), "Nesting Levels have exceeded supported limits")
}
