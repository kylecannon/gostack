package value_test

import (
	"runtime/debug"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

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
