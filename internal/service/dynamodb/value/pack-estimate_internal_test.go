package value

import (
	"strconv"
	"strings"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/stretchr/testify/require"
)

// Test_packShape_coversTheJSON: the size Pack grows its scratch buffer to
// holds the item's json, so a buffer the pool lost grows once, and is not far
// over it, so a large item does not take much more scratch than its json.
// Every kind of value is covered, alone and many at once.
func Test_packShape_coversTheJSON(t *testing.T) {
	str := func(s string) *string { return &s }
	big := strings.Repeat("x", 64<<10)

	many := Item{}
	for i := range 2000 {
		many["a"+strconv.Itoa(i)] = api.AttributeValue{S: str("v")}
	}

	list := make([]api.AttributeValue, 1000)
	for i := range list {
		list[i] = api.AttributeValue{N: str(strconv.Itoa(i * 7919))}
	}

	items := map[string]Item{
		"string":     {"pk": {S: str(big)}},
		"number":     {"pk": {S: str("k")}, "n": {N: str("-1234567890.123456789e-10")}},
		"binary":     {"pk": {S: str("k")}, "b": {B: []byte(big)}},
		"attributes": many,
		"list":       {"pk": {S: str("k")}, "l": {L: list}},
		"map": {"pk": {S: str("k")}, "m": {M: map[string]api.AttributeValue{
			"x": {M: map[string]api.AttributeValue{"y": {BOOL: new(true)}, "z": {NULL: new(true)}}},
		}}},
		"sets": {
			"pk": {S: str("k")}, "ss": {SS: []string{"a", "bb", ""}}, "ns": {NS: []string{"1", "22.5", "-3e7"}},
			"bs": {BS: [][]byte{{1}, {2, 3}, []byte(big[:100])}},
		},
	}

	for name, item := range items {
		packed, err := Pack(item)
		require.NoError(t, err, name)

		h := packed.header()
		json := len(packed) - h.body

		var shape packShape
		shape.item(item)

		estimate := ItemSize(item) + shape.extra + packWrapping
		require.GreaterOrEqual(t, estimate, json, "%s: the estimate holds the json", name)
		require.LessOrEqual(t, estimate, json+json/2+packWrapping, "%s: the estimate is near the json", name)
	}
}
