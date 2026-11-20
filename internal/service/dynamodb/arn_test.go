package dynamodb_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/protocol"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service/dynamodb"
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

// Test_arn_partition: a table's ARN, its stream's ARN and its KMS key's ARN
// name the partition of the region of the call (account.Partition). The
// gateway answers a region outside the aws partition as us-east-1, so the
// test hands the provider's dispatcher a request that already carries the
// region.
func Test_arn_partition(t *testing.T) {
	for region, partition := range map[string]string{
		usEast1: "aws", "cn-north-1": "aws-cn", "us-gov-west-1": "aws-us-gov", "us-iso-east-1": "aws-iso",
	} {
		t.Run(region, func(t *testing.T) {
			serve := dynamodb.New(func() dynamodb.Settings { return dynamodb.Settings{} }, nil).Registration().Serve

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(
				`{"TableName":"parted","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],`+
					`"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],"BillingMode":"PAY_PER_REQUEST",`+
					`"StreamSpecification":{"StreamEnabled":true,"StreamViewType":"KEYS_ONLY"},`+
					`"SSESpecification":{"Enabled":true,"SSEType":"KMS","KMSMasterKeyId":"k1"}}`))
			req.Header.Set("Content-Type", jsonType)
			req.Header.Set("X-Amz-Target", "DynamoDB_20120810."+opCreate)

			rec := httptest.NewRecorder()
			serve(rec, &request.Context{
				Service: ddbName, Operation: opCreate, Protocol: protocol.KindJSON, AccountID: accountID,
				Region: region, RequestID: "00000000-0000-4000-8000-000000000000", Request: req,
			})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			var out map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))

			prefix := "arn:" + partition + ":dynamodb:" + region + ":" + accountID + ":table/parted"
			require.Equal(t, prefix, path(t, out, keyDescription, "TableArn"))
			require.Contains(t, path(t, out, keyDescription, "LatestStreamArn"), prefix+"/stream/")
			require.Equal(t, "arn:"+partition+":kms:"+region+":"+accountID+":key/k1",
				path(t, out, keyDescription, "SSEDescription", "KMSMasterKeyArn"))
		})
	}
}
