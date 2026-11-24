package value_test

import (
	"strconv"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

// Test_AppendJSON_mapMembersStayOnTheStack: the generated encoder writes a map
// of AttributeValues without an allocation for each member. The range variable
// of such a map has its address passed to AttributeValue's encoder, which
// recurses through M, so it escapes and costs one allocation a member unless
// the generator passes it by value through appendJSONMapValueAttributeValue
// (SQ-526). The bench gate builds main and the branch on the same toolchain, so
// a toolchain or generator change that brings the escape back shows only here.
// The encoder writes into a buffer already large enough, with no pool in the
// path, so the count is the encoder's own.
func Test_AppendJSON_mapMembersStayOnTheStack(t *testing.T) { //nolint:paralleltest // AllocsPerRun is not parallel
	const members = 64

	inner := make(value.Item, members)
	for i := range members {
		inner["m"+strconv.Itoa(i)] = api.AttributeValue{S: new("v")}
	}

	item := make(value.Item, members)
	for i := range members {
		item["a"+strconv.Itoa(i)] = api.AttributeValue{M: map[string]api.AttributeValue{"x": {M: inner}}}
	}

	out := api.GetItemOutput{Item: item}
	buf := make([]byte, 0, 1<<20)

	allocs := testing.AllocsPerRun(10, func() {
		_, err := out.AppendJSON(buf[:0])
		if err != nil {
			panic(err)
		}
	})

	// The item has 64 + 64 + 64*64 map members; an escape allocates once for
	// each of them.
	require.Less(t, allocs, float64(members), "allocations of one encode of the nested item")
}
