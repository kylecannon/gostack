package dynamodb_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// arnt is the ARN of the test table arnt; another account's or region's ARN
// names the caller's table of that name too, as the LocalStack image answers.
const (
	arnt       = "arn:aws:dynamodb:us-east-1:000000000000:table/arnt"
	arntAbroad = "arn:aws:dynamodb:eu-west-1:111111111111:table/arnt"
)

// Test_arn_tableName: every item and table operation takes the table's ARN
// as its name and names the table in what it answers, as the LocalStack image
// does (verification/SQ-302 q3).
func Test_arn_tableName(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("arnt"))

	cap1 := `"ConsumedCapacity":{"CapacityUnits":1,"TableName":"arnt"}`
	half := `"ConsumedCapacity":{"CapacityUnits":0.5,"TableName":"arnt"}`
	stored := `{"id":{"S":"1"},"v":{"S":"a"}}`

	for _, c := range []struct{ op, body, want string }{
		{opPut, `{"TableName":"` + arnt + `","Item":` + stored + `,"ReturnConsumedCapacity":"TOTAL"}`, `{` + cap1 + `}`},
		{
			opGet, `{"TableName":"` + arntAbroad + `","Key":{"id":{"S":"1"}},"ReturnConsumedCapacity":"TOTAL"}`,
			`{` + half + `,"Item":` + stored + `}`,
		},
		{
			opQuery, `{"TableName":"` + arnt + `","KeyConditionExpression":"id = :i",` +
				`"ExpressionAttributeValues":{":i":{"S":"1"}},"ReturnConsumedCapacity":"TOTAL"}`,
			`{` + half + `,"Count":1,"ScannedCount":1,"Items":[` + stored + `]}`,
		},
		{
			opScan, `{"TableName":"` + arnt + `","ReturnConsumedCapacity":"TOTAL"}`,
			`{` + half + `,"Count":1,"ScannedCount":1,"Items":[` + stored + `]}`,
		},
		{
			"UpdateItem", `{"TableName":"` + arnt + `","Key":{"id":{"S":"2"}},"ReturnConsumedCapacity":"TOTAL"}`,
			`{` + cap1 + `}`,
		},
		{
			opDeleteI, `{"TableName":"` + arnt + `","Key":{"id":{"S":"2"}},"ReturnConsumedCapacity":"TOTAL"}`,
			`{` + cap1 + `}`,
		},
		{
			opBatchGet, `{"RequestItems":{"` + arnt + `":{"Keys":[{"id":{"S":"1"}}]}},"ReturnConsumedCapacity":"TOTAL"}`,
			`{"Responses":{"arnt":[` + stored + `]},"UnprocessedKeys":{},` +
				`"ConsumedCapacity":[{"CapacityUnits":0.5,"TableName":"arnt"}]}`,
		},
		{
			opBatchWrite, `{"RequestItems":{"` + arnt + `":[{"PutRequest":{"Item":{"id":{"S":"3"}}}}]},` +
				`"ReturnConsumedCapacity":"TOTAL"}`,
			`{"UnprocessedItems":{},"ConsumedCapacity":[{"CapacityUnits":1,"TableName":"arnt"}]}`,
		},
	} {
		got := s.call(t, usEast1, c.op, c.body)
		require.Equal(t, http.StatusOK, got.status, "%s: %s", c.op, got.body)
		require.JSONEq(t, c.want, got.body, c.op)
	}

	described := s.ok(t, "DescribeTable", `{"TableName":"`+arnt+`"}`)
	require.Equal(t, arnt, path(t, described, "Table", "TableArn"))

	s.fails(t, opGet, `{"TableName":"`+arnt+`/index/x","Key":{"id":{"S":"1"}}}`, codeValidation,
		"Invalid table/index name.  Table/index names must be between 3 and 255 characters long, "+
			"and may contain only the characters a-z, A-Z, 0-9, '_', '-', and '.'")
	s.fails(t, opBatchGet, `{"RequestItems":{"arn:aws:dynamodb:us-east-1:000000000000:table/nope":`+
		`{"Keys":[{"id":{"S":"1"}}]}}}`, "ResourceNotFoundException", "Cannot do operations on a non-existent table")
}

// Test_arn_tokenReplay: a transaction sent with a token naming its table by
// ARN, then sent again naming it by name, replays, as on the LocalStack image
// (verification/SQ-293 p2): the token's fingerprint takes the resolved table.
func Test_arn_tokenReplay(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("arnt"))

	send := func(name, v string) reply {
		return s.call(t, usEast1, opTransactWrite, `{"ClientRequestToken":"arn-token","TransactItems":[`+
			`{"Put":{"TableName":"`+name+`","Item":{"id":{"S":"1"},"v":{"S":"`+v+`"}}}}]}`)
	}

	require.Equal(t, http.StatusOK, send(arnt, "a").status)
	s.ok(t, opPut, `{"TableName":"arnt","Item":{"id":{"S":"1"},"v":{"S":"later"}}}`)

	got := send("arnt", "a")
	require.Equal(t, http.StatusOK, got.status, got.body)
	require.JSONEq(t, `{"Item":{"id":{"S":"1"},"v":{"S":"later"}}}`,
		s.call(t, usEast1, opGet, `{"TableName":"arnt","Key":{"id":{"S":"1"}}}`).body, "the replay wrote nothing")
}
