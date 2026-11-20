package expression_test

import (
	"fmt"
	"runtime/debug"
	"strings"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/expression"
)

const (
	tooDeepValue = "ExpressionAttributeValues contains invalid value: Nesting Levels have exceeded supported " +
		"limits: Attributes in the item have nested levels beyond supported limit for key :x"
	tooDeepItem = "Nesting Levels have exceeded supported limits"
	setX        = "SET a = :x"
)

// nest is a string wrapped in lists, maps or both (kind "L", "M", "LM"), so
// that it counts the given number of levels, the string included. Every value the image was
// probed with (SQ-280) is one of these.
func nest(kind string, levels int) api.AttributeValue {
	v := str("x")

	for i := 1; i < levels; i++ {
		switch kind {
		case "L":
			v = api.AttributeValue{L: []api.AttributeValue{v}}
		case "M":
			v = api.AttributeValue{M: map[string]api.AttributeValue{"k": v}}
		default:
			if i%2 == 1 {
				v = api.AttributeValue{L: []api.AttributeValue{v}}
			} else {
				v = api.AttributeValue{M: map[string]api.AttributeValue{"k": v}}
			}
		}
	}

	return v
}

// Test_CheckSubstitutions_valueNesting: a value in ExpressionAttributeValues
// nests at most 33 levels, one more than an item's attribute, as the image
// answers for lists, maps and both: 33 levels pass, 34 are refused naming the
// key (SQ-280; probes of the image at 31 to 100 levels).
func Test_CheckSubstitutions_valueNesting(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"L", "M", "LM"} {
		for _, tt := range []struct {
			levels int
			want   string
		}{
			{31, ""}, {32, ""}, {33, ""}, {34, tooDeepValue}, {35, tooDeepValue}, {100, tooDeepValue},
		} {
			t.Run(fmt.Sprintf("%s/%d levels", kind, tt.levels), func(t *testing.T) {
				t.Parallel()

				subs := expression.Substitutions{Values: map[string]api.AttributeValue{":x": nest(kind, tt.levels)}}
				wantMessage(t, expression.CheckSubstitutions(subs), tt.want)
			})
		}
	}
}

// Test_CheckSubstitutions_deepBounded: a value nested 100,000 levels deep is
// refused within a 256 KiB stack, so neither the copy CheckSubstitutions
// validates nor the validation descends past the limit. An unbounded
// recursion would pass the stack limit, which ends the test binary.
//
// It does not run in parallel: SetMaxStack applies to every goroutine, and Go
// starts the parallel tests only after the serial ones finish.
//
//nolint:paralleltest // SetMaxStack is process-wide; see above.
func Test_CheckSubstitutions_deepBounded(t *testing.T) {
	const levels = 100_000

	v := str("x")
	for range levels {
		v = api.AttributeValue{L: []api.AttributeValue{v}}
	}

	subs := expression.Substitutions{Values: map[string]api.AttributeValue{":x": v}}

	const stack = 256 << 10

	defer debug.SetMaxStack(debug.SetMaxStack(stack))

	wantMessage(t, expression.CheckSubstitutions(subs), tooDeepValue)

	_, err := expression.ParseUpdate(setX, subs, nil)
	wantMessage(t, err, tooDeepValue)
}

// Test_UpdateExpr_Apply_nesting: the item an update makes may nest an
// attribute at most 32 levels, though the value it sets may nest 33. The
// image refuses the one level too many with no key in the message, for SET of
// a value, of list_append's result, and of a nested path, and takes the
// levels before it (SQ-280).
func Test_UpdateExpr_Apply_nesting(t *testing.T) {
	t.Parallel()

	chain := mapJSON

	for _, tt := range []struct {
		name  string
		item  string
		expr  string
		subs  map[string]api.AttributeValue
		wantE string
	}{
		{"set 32 levels", updateKey, setX, map[string]api.AttributeValue{":x": nest("L", 32)}, ""},
		{"set 33 levels", updateKey, setX, map[string]api.AttributeValue{":x": nest("L", 33)}, tooDeepItem},
		{"set 33 mixed levels", updateKey, setX, map[string]api.AttributeValue{":x": nest("LM", 33)}, tooDeepItem},
		{
			"list_append 31 levels in the list", updateKey, "SET a = list_append(:y, :x)",
			map[string]api.AttributeValue{":y": {L: []api.AttributeValue{}}, ":x": {L: []api.AttributeValue{nest("L", 31)}}},
			"",
		},
		{
			"list_append 32 levels in the list", updateKey, "SET a = list_append(:y, :x)",
			map[string]api.AttributeValue{":y": {L: []api.AttributeValue{}}, ":x": {L: []api.AttributeValue{nest("L", 32)}}},
			tooDeepItem,
		},
		{
			"set a leaf of a 31-level map to 2 levels", `{"pk":{"S":"k"},"d":` + chain(31) + `}`,
			"SET d" + strings.Repeat(".k", 30) + " = :x",
			map[string]api.AttributeValue{":x": nest("M", 2)},
			"",
		},
		{
			"set a leaf of a 31-level map to 3 levels", `{"pk":{"S":"k"},"d":` + chain(31) + `}`,
			"SET d" + strings.Repeat(".k", 30) + " = :x",
			map[string]api.AttributeValue{":x": nest("M", 3)},
			tooDeepItem,
		},
		{
			"set a list element to 31 levels", `{"pk":{"S":"k"},"l":` + listJSON(20) + `}`,
			"SET l[0] = :x",
			map[string]api.AttributeValue{":x": nest("L", 31)},
			"",
		},
		{
			"set a list element to 32 levels", `{"pk":{"S":"k"},"l":` + listJSON(20) + `}`,
			"SET l[0] = :x",
			map[string]api.AttributeValue{":x": nest("L", 32)},
			tooDeepItem,
		},
		{
			"remove inside a 32-level attribute", `{"pk":{"S":"k"},"d":` + chain(32) + `}`,
			"REMOVE d" + strings.Repeat(".k", 31), nil, "",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			old := updateItem(t, tt.item)
			_, _, err := updateApply(t, old, tt.expr, expression.Substitutions{Values: tt.subs})
			wantMessage(t, err, tt.wantE)
		})
	}
}

// mapJSON is the JSON of a string inside levels-1 maps keyed "k".
func mapJSON(levels int) string {
	v := `{"S":"x"}`
	for i := 1; i < levels; i++ {
		v = `{"M":{"k":` + v + `}}`
	}

	return v
}

// listJSON is the JSON of a string inside levels-1 lists.
func listJSON(levels int) string {
	v := `{"S":"x"}`
	for i := 1; i < levels; i++ {
		v = `{"L":[` + v + `]}`
	}

	return v
}
