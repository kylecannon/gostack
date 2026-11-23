package dynamodb_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// DynamoDB tells a json null from an absent entry: its own messages for a null
// attribute name, string set member and attribute value list element are not
// the answer to the entry left out. gostack does not match those messages yet,
// but it must not drop the null and answer for another request, so DynamoDB and
// DynamoDB Streams decode with awsjson.KeepNulls (serviceOptions.KeepNulls in the
// generator) and answer as they did before nulls were dropped.
func Test_a_null_in_a_dynamodb_request_is_not_dropped(t *testing.T) {
	t.Parallel()

	s := newStack(t)
	s.ok(t, "CreateTable", hashTable("tab1"))

	for _, tc := range []struct {
		name    string
		body    string
		status  int
		code    string
		message string
	}{
		{
			name: "a null expression attribute name",
			body: `{"TableName":"tab1","Item":{"id":{"S":"a"}},"ConditionExpression":"attribute_not_exists(#n)",` +
				`"ExpressionAttributeNames":{"#n":null}}`,
			status:  http.StatusBadRequest,
			code:    codeValidation,
			message: "ExpressionAttributeNames contains invalid value: Empty attribute name for key #n",
		},
		{
			name: "a null beside a defined expression attribute name",
			body: `{"TableName":"tab1","Item":{"id":{"S":"a"}},"ConditionExpression":"attribute_not_exists(#n)",` +
				`"ExpressionAttributeNames":{"#n":null,"#m":"id"}}`,
			status:  http.StatusBadRequest,
			code:    codeValidation,
			message: "ExpressionAttributeNames contains invalid value: Empty attribute name for key #n",
		},
		{
			name:    "a null number set member",
			body:    `{"TableName":"tab1","Item":{"id":{"S":"b"},"ns":{"NS":["1",null]}}}`,
			status:  http.StatusBadRequest,
			code:    codeValidation,
			message: msgAuBadNumber,
		},
		{
			name:   "a null string set member",
			body:   `{"TableName":"tab1","Item":{"id":{"S":"c"},"ss":{"SS":[null]}}}`,
			status: http.StatusOK,
		},
		{
			name: "a null attribute value list element",
			body: `{"TableName":"tab1","Item":{"id":{"S":"d"}},"Expected":{"x":{"ComparisonOperator":"IN",` +
				`"AttributeValueList":[null]}}}`,
			status:  http.StatusBadRequest,
			code:    codeValidation,
			message: msgAuEmpty,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := s.call(t, usEast1, "PutItem", tc.body)
			assert.Equal(t, tc.status, got.status, got.body)

			if tc.code != "" {
				assert.JSONEq(t, `{"__type":"`+tc.code+`","message":"`+tc.message+`"}`, got.body)
			}
		})
	}
}
