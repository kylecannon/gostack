package dynamodb_test

import (
	"fmt"
	"math"
	"strings"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingHold records the most a decoding charged.
type recordingHold struct{ cost int64 }

func (h *recordingHold) HoldDecode(cost int64) (int64, error) {
	h.cost = max(h.cost, cost)

	return cost, nil
}

// Test_decodeBudget_declarationCoversTheWorstShapes measures, for each shape of
// small values that DynamoDB's limits allow in bulk, what decoding charges for
// each byte of body and for each element (a comma or an opening bracket), and
// holds both to what the provider declares: 256 times the body
// (service.Registration.DecodeFactor) and 1,700 for an element (DecodeUnit).
// docs/architecture.md, "DynamoDB's decode budget", lists the numbers; this
// fails when the decoder's cost model passes either declaration.
func Test_decodeBudget_declarationCoversTheWorstShapes(t *testing.T) {
	const elements = 20_000

	reg := newStack(t).provider.Registration()

	var wide, record strings.Builder

	for i := range elements {
		fmt.Fprintf(&wide, `,"a%d":{"N":"1"}`, i)
	}

	for i := range elements {
		if i > 0 {
			record.WriteString(",")
		}

		fmt.Fprintf(&record, `"k%d":{"BOOL":true}`, i)
	}

	nest := strings.Repeat(`{"L":[`, 30) + `{"S":""}` + strings.Repeat(`]}`, 30)

	shapes := map[string]string{
		"nested to the depth limit": listItem(nest, elements/20),
		"S":                         listItem(`{"S":""}`, elements),
		"B":                         listItem(`{"B":""}`, elements),
		"empty L":                   listItem(`{"L":[]}`, elements),
		"empty M":                   listItem(`{"M":{}}`, elements),
		"N":                         listItem(`{"N":"1"}`, elements),
		"NS":                        listItem(`{"NS":["1"]}`, elements),
		"SS":                        listItem(`{"SS":["a"]}`, elements),
		"BOOL":                      listItem(`{"BOOL":true}`, elements),
		"NULL":                      listItem(`{"NULL":true}`, elements),
		"attributes":                `{"TableName":"t","Item":{"id":{"S":"1"}` + wide.String() + `}}`,
		"map entries":               `{"TableName":"t","Item":{"id":{"S":"1"},"m":{"M":{` + record.String() + `}}}}`,
	}

	for name, body := range shapes {
		t.Run(name, func(t *testing.T) {
			var hold recordingHold

			_, _, err := awsjson.DecodeWithin[api.PutItemInput]([]byte(body), math.MaxInt64, &hold)
			require.Nil(t, err)

			separators := strings.Count(body, ",") + strings.Count(body, "[")

			assert.Less(t, float64(hold.cost)/float64(len(body)), float64(reg.DecodeFactor), "charged per byte of body")
			assert.LessOrEqual(t, float64(hold.cost)/float64(separators), float64(reg.DecodeUnit), "charged per element")
		})
	}
}

// listItem is a PutItem body whose attribute l is count copies of element.
func listItem(element string, count int) string {
	return `{"TableName":"t","Item":{"id":{"S":"1"},"l":{"L":[` +
		strings.TrimSuffix(strings.Repeat(element+",", count), ",") + `]}}}`
}
