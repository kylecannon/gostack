package dynamodb_test

import (
	"fmt"
	"strings"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/protocol/awsjson"
	"github.com/stretchr/testify/assert"
)

// Test_decodeBudget_declarationCoversTheWorstShapes holds what decoding charges
// for each byte of body, for each shape of small values that DynamoDB's limits
// allow in bulk, to what the provider declares: 256 times the body
// (service.Registration.DecodeFactor). docs/architecture.md, "DynamoDB's
// decode budget", lists the numbers; this fails when the decoder's cost model
// passes the declaration.
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
			// A budget of the factor times the body, without DecodeSlack,
			// refuses a body charged more for each of its bytes.
			_, _, err := awsjson.DecodeWithin[api.PutItemInput]([]byte(body), int64(reg.DecodeFactor)*int64(len(body)), nil)
			assert.Nil(t, err, "charged less than the declared factor for each byte of body")
		})
	}
}

// listItem is a PutItem body whose attribute l is count copies of element.
func listItem(element string, count int) string {
	return `{"TableName":"t","Item":{"id":{"S":"1"},"l":{"L":[` +
		strings.TrimSuffix(strings.Repeat(element+",", count), ",") + `]}}}`
}
