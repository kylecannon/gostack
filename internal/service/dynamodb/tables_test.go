package dynamodb_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	opCreate   = "CreateTable"
	opDescribe = "DescribeTable"
	opDelete   = "DeleteTable"
	opList     = "ListTables"
	opUpdate   = "UpdateTable"

	codeValidation = "ValidationException"
	codeNotFound   = "ResourceNotFoundException"
	keyTable       = "Table"
	keyDescription = "TableDescription"
	keyWarm        = "WarmThroughput"
	keyStatus      = "TableStatus"
	keyGSIs        = "GlobalSecondaryIndexes"
	keyBilling     = "BillingModeSummary"
	keyThroughput  = "ProvisionedThroughput"
	keyNames       = "TableNames"
	statusActive   = "ACTIVE"
	tab1           = "tab1"
	describeTab1   = `{"TableName":"tab1"}`
	arnPrefix      = "arn:aws:dynamodb:us-east-1:" + accountID + ":table/"

	idKey      = `"KeySchema":[{"AttributeName":"id","KeyType":"HASH"}]`
	idDefs     = `"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}]`
	throughput = `"ProvisionedThroughput":{"ReadCapacityUnits":5,"WriteCapacityUnits":5}`
)

// body joins JSON members into an object.
func body(members ...string) string {
	return "{" + strings.Join(members, ",") + "}"
}

func named(name string) string {
	return `"TableName":"` + name + `"`
}

// Test_tableCrud is test_table_crud: the create, describe and delete shapes
// AWS's snapshot pins, and the delete of a table that does not exist.
func Test_tableCrud(t *testing.T) {
	s := newStack(t)

	d := path(t, s.ok(t, opCreate, hashTable(tab1)), keyDescription)
	require.Equal(t, statusActive, path(t, d, keyStatus))
	require.Equal(t, arnPrefix+tab1, path(t, d, "TableArn"))
	require.Equal(t, map[string]any{"NumberOfDecreasesToday": 0.0, "ReadCapacityUnits": 5.0, "WriteCapacityUnits": 5.0},
		path(t, d, keyThroughput))
	require.Len(t, path(t, d, "TableId"), 36)
	require.Equal(t, false, path(t, d, "DeletionProtectionEnabled"))

	for _, absent := range []string{keyWarm, keyBilling, "SSEDescription", "TableClassSummary", keyGSIs} {
		require.NotContains(t, d, absent)
	}

	require.Equal(t, []any{tab1}, s.ok(t, opList, `{}`)[keyNames])

	described := path(t, s.ok(t, opDescribe, describeTab1), keyTable)
	require.Equal(t, map[string]any{"ReadUnitsPerSecond": 5.0, "WriteUnitsPerSecond": 5.0, "Status": statusActive},
		path(t, described, keyWarm))
	require.Equal(t, path(t, d, "TableId"), path(t, described, "TableId"))

	deleted := path(t, s.ok(t, opDelete, describeTab1), keyDescription)
	require.Equal(t, "DELETING", path(t, deleted, keyStatus))
	require.NotContains(t, deleted, keyWarm)

	require.Equal(t, []any{}, s.ok(t, opList, `{}`)[keyNames])
	s.fails(t, opDelete, `{"TableName":"non-existent"}`, codeNotFound,
		"Requested resource not found: Table: non-existent not found")
	s.fails(t, opDescribe, describeTab1, codeNotFound, "Cannot do operations on a non-existent table")
}

// Test_createTable_errors holds CreateTable's messages, as DynamoDB Local and
// LocalStack answer them (test_create_duplicate_table,
// test_dynamodb_pay_per_request).
func Test_createTable_errors(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable(tab1))

	tab2 := named("tab2")
	lsi := `"LocalSecondaryIndexes":[{"IndexName":"lix","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],` +
		`"Projection":{"ProjectionType":"ALL"}}]`
	twoDefs := `"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"},` +
		`{"AttributeName":"x","AttributeType":"S"}]`

	tests := []struct {
		name, body, code, message string
	}{
		{"duplicate", hashTable(tab1), "ResourceInUseException", "Table already exists: tab1"},
		{
			"pay per request with throughput",
			body(tab2, idKey, idDefs, throughput, `"BillingMode":"PAY_PER_REQUEST"`), codeValidation,
			"One or more parameter values were invalid: Neither ReadCapacityUnits nor WriteCapacityUnits " +
				"can be specified when BillingMode is PAY_PER_REQUEST",
		},
		{
			"bad name", body(named("t$"), idKey, idDefs, throughput), codeValidation,
			"Invalid table/index name.  Table/index names must be between 3 and 255 characters long, " +
				"and may contain only the characters a-z, A-Z, 0-9, '_', '-', and '.'",
		},
		{
			"no throughput", body(tab2, idKey, idDefs), codeValidation,
			"No provisioned throughput specified for the table",
		},
		{
			"no key schema", body(tab2, idDefs, throughput), codeValidation,
			"No defined key schema.  A key schema containing at least a hash key must be defined for all tables",
		},
		{
			"undefined hash", body(tab2, idKey, `"AttributeDefinitions":[]`, throughput), codeValidation,
			"Hash Key not specified in Attribute Definitions.  Type unknown.",
		},
		{
			"extra definition", body(tab2, idKey, twoDefs, throughput), codeValidation,
			"The number of attributes in key schema must match the number of attributes defined in " +
				"attribute definitions.",
		},
		{
			"bool key", body(tab2, idKey, `"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"BOOL"}]`,
				throughput), codeValidation, "Member must satisfy enum value set: [B, N, S]",
		},
		{
			"local index on hash table", body(tab2, idKey, idDefs, throughput, lsi), codeValidation,
			"Local Secondary indices are not allowed on hash tables, only hash and range tables",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s.fails(t, opCreate, tt.body, tt.code, tt.message)
		})
	}
}

// gsiTable is test_gsi_with_billing_mode's CreateTable body.
func gsiTable(mode string) string {
	index := `"GlobalSecondaryIndexes":[{"IndexName":"TransactionRecordID",` +
		`"KeySchema":[{"AttributeName":"TRID","KeyType":"HASH"}],"Projection":{"ProjectionType":"ALL"}`
	members := []string{
		named("g-" + mode), `"KeySchema":[{"AttributeName":"TID","KeyType":"HASH"}]`,
		`"AttributeDefinitions":[{"AttributeName":"TID","AttributeType":"S"},{"AttributeName":"TRID","AttributeType":"S"}]`,
		`"BillingMode":"` + mode + `"`,
	}

	if mode == "PROVISIONED" {
		members = append(members, throughput,
			index+`,"ProvisionedThroughput":{"ReadCapacityUnits":1,"WriteCapacityUnits":1}}]`)
	} else {
		members = append(members, index+"}]")
	}

	return body(members...)
}

// Test_createTable_gsiBilling is test_gsi_with_billing_mode: the GSI fields
// LocalStack adds, for both billing modes.
func Test_createTable_gsiBilling(t *testing.T) {
	s := newStack(t)

	ppr := path(t, s.ok(t, opCreate, gsiTable("PAY_PER_REQUEST")), keyDescription)
	require.Equal(t, map[string]any{"BillingMode": "PAY_PER_REQUEST"}, path(t, ppr, keyBilling))
	require.InDelta(t, 0.0, path(t, ppr, keyThroughput, "ReadCapacityUnits"), 0)

	index := path(t, ppr, keyGSIs, 0)
	require.Equal(t, statusActive, path(t, index, "IndexStatus"))
	require.Equal(t, arnPrefix+"g-PAY_PER_REQUEST/index/TransactionRecordID", path(t, index, "IndexArn"))
	require.Equal(t, map[string]any{"NumberOfDecreasesToday": 0.0, "ReadCapacityUnits": 0.0, "WriteCapacityUnits": 0.0},
		path(t, index, keyThroughput))

	described := path(t, s.ok(t, opDescribe, `{"TableName":"g-PAY_PER_REQUEST"}`), keyTable)
	require.InDelta(t, 12000.0, path(t, described, keyWarm, "ReadUnitsPerSecond"), 0)
	require.NotNil(t, path(t, described, keyBilling, "LastUpdateToPayPerRequestDateTime"))

	prov := path(t, s.ok(t, opCreate, gsiTable("PROVISIONED")), keyDescription)
	require.NotContains(t, prov, keyBilling)
	require.InDelta(t, 1.0, path(t, prov, keyGSIs, 0, keyThroughput, "ReadCapacityUnits"), 0)
}

// Test_createTable_extras holds LocalStack's other additions: TableClassSummary
// and its update, SSEDescription with a key ARN, and WarmThroughput
// (test_table_warm_throughput, test_dynamodb_create_table_with_class,
// test_dynamodb_create_table_with_sse_specification).
func Test_createTable_extras(t *testing.T) {
	s := newStack(t)

	warm := path(t, s.ok(t, opCreate, body(named("warm"), idKey, idDefs, throughput,
		`"WarmThroughput":{"ReadUnitsPerSecond":1000,"WriteUnitsPerSecond":1200}`, `"TableClass":"STANDARD"`,
		`"SSESpecification":{"Enabled":true,"SSEType":"KMS","KMSMasterKeyId":"k1"}`)), keyDescription)
	require.Equal(t, "UPDATING", path(t, warm, keyWarm, "Status"))
	require.Equal(t, "STANDARD", path(t, warm, "TableClassSummary", "TableClass"))
	require.Equal(t, map[string]any{
		"Status": "ENABLED", "SSEType": "KMS", "KMSMasterKeyArn": "arn:aws:kms:us-east-1:" + accountID + ":key/k1",
	}, path(t, warm, "SSEDescription"))

	described := path(t, s.ok(t, opDescribe, `{"TableName":"warm"}`), keyTable)
	require.InDelta(t, 1200.0, path(t, described, keyWarm, "WriteUnitsPerSecond"), 0)
	require.Equal(t, statusActive, path(t, described, keyWarm, "Status"))

	updated := s.ok(t, opUpdate, `{"TableName":"warm","TableClass":"STANDARD_INFREQUENT_ACCESS"}`)
	require.Equal(t, "STANDARD_INFREQUENT_ACCESS", path(t, updated, keyDescription, "TableClassSummary", "TableClass"))
}

// Test_createTable_manyIndexes is test_more_than_20_global_secondary_indexes:
// no cap on the number of global secondary indexes.
func Test_createTable_manyIndexes(t *testing.T) {
	s := newStack(t)

	defs := make([]string, 0, 26)
	defs = append(defs, `{"AttributeName":"id","AttributeType":"S"}`)
	indexes := make([]string, 0, 25)

	for i := range 25 {
		n := strconv.Itoa(i)
		defs = append(defs, `{"AttributeName":"a`+n+`","AttributeType":"S"}`)
		indexes = append(indexes, `{"IndexName":"gsi_`+n+`","KeySchema":[{"AttributeName":"a`+n+
			`","KeyType":"HASH"}],"Projection":{"ProjectionType":"ALL"}}`)
	}

	s.ok(t, opCreate, body(named("many"), idKey, `"AttributeDefinitions":[`+strings.Join(defs, ",")+`]`,
		`"GlobalSecondaryIndexes":[`+strings.Join(indexes, ",")+`]`, `"BillingMode":"PAY_PER_REQUEST"`))
	require.Len(t, path(t, s.ok(t, opDescribe, `{"TableName":"many"}`), keyTable, keyGSIs), 25)
}

// Test_region_localhost is test_nosql_workbench_localhost_region: "localhost"
// and "local" are us-east-1.
func Test_region_localhost(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable(tab1))

	for _, region := range []string{"localhost", "local"} {
		got := s.call(t, region, opDescribe, describeTab1)
		require.Equal(t, http.StatusOK, got.status, got.body)
	}

	got := s.call(t, "eu-west-1", opDescribe, describeTab1)
	require.Equal(t, http.StatusBadRequest, got.status, got.body)
}

// Test_listTables_pages: Limit pages through the names in order, and
// LastEvaluatedTableName is there only while names remain.
func Test_listTables_pages(t *testing.T) {
	s := newStack(t)
	for _, name := range []string{"ccc", "aaa", "bbb"} {
		s.ok(t, opCreate, hashTable(name))
	}

	first := s.ok(t, opList, `{"Limit":2}`)
	require.Equal(t, []any{"aaa", "bbb"}, first[keyNames])
	require.Equal(t, "bbb", first["LastEvaluatedTableName"])

	rest := s.ok(t, opList, `{"Limit":2,"ExclusiveStartTableName":"bbb"}`)
	require.Equal(t, []any{"ccc"}, rest[keyNames])
	require.NotContains(t, rest, "LastEvaluatedTableName")

	s.fails(t, opList, `{"Limit":0}`, codeValidation, "Limit must be greater than or equal to 1")
	s.fails(t, opList, `{"Limit":101}`, codeValidation, "Limit in ListTables must be <= 100")
}

// Test_updateTable_indexes: an index UpdateTable creates answers CREATING,
// is ACTIVE from the next call, is filled from the items stored, and can be
// deleted; the billing mode switches back to provisioned only with throughput.
func Test_updateTable_indexes(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, body(named(tab1), idKey, idDefs, `"BillingMode":"PAY_PER_REQUEST"`))
	s.ok(t, opPut, `{"TableName":"tab1","Item":{"id":{"S":"a"},"q":{"S":"x"}}}`)

	create := body(named(tab1), `"AttributeDefinitions":[{"AttributeName":"q","AttributeType":"S"}]`,
		`"GlobalSecondaryIndexUpdates":[{"Create":{"IndexName":"qix",`+
			`"KeySchema":[{"AttributeName":"q","KeyType":"HASH"}],"Projection":{"ProjectionType":"ALL"}}}]`)
	created := path(t, s.ok(t, opUpdate, create), keyDescription, keyGSIs, 0)
	require.Equal(t, "CREATING", path(t, created, "IndexStatus"))
	require.Equal(t, false, path(t, created, "Backfilling"))

	index := path(t, s.ok(t, opDescribe, describeTab1), keyTable, keyGSIs, 0)
	require.Equal(t, statusActive, path(t, index, "IndexStatus"))
	require.InDelta(t, 1.0, path(t, index, "ItemCount"), 0)

	deleteIndex := func(name string) string {
		return body(named(tab1), `"GlobalSecondaryIndexUpdates":[{"Delete":{"IndexName":"`+name+`"}}]`)
	}
	s.fails(t, opUpdate, deleteIndex("nope"), codeNotFound, "Requested resource not found")
	s.ok(t, opUpdate, deleteIndex("qix"))
	require.NotContains(t, path(t, s.ok(t, opDescribe, describeTab1), keyTable), keyGSIs)

	s.fails(t, opUpdate, body(named(tab1), `"BillingMode":"PROVISIONED"`), codeValidation,
		"No provisioned throughput specified for the table")

	switched := path(t, s.ok(t, opUpdate, body(named(tab1), `"BillingMode":"PROVISIONED"`, throughput)), keyDescription)
	require.NotContains(t, switched, keyBilling)
	require.InDelta(t, 5.0, path(t, switched, keyThroughput, "WriteCapacityUnits"), 0)
}

// Test_limitsAndEndpoints: DescribeLimits answers DynamoDB Local's numbers and
// DescribeEndpoints gostack's address.
func Test_limitsAndEndpoints(t *testing.T) {
	s := newStack(t)

	require.Equal(t, map[string]any{
		"AccountMaxReadCapacityUnits": 80000.0, "AccountMaxWriteCapacityUnits": 80000.0,
		"TableMaxReadCapacityUnits": 40000.0, "TableMaxWriteCapacityUnits": 40000.0,
	}, s.ok(t, "DescribeLimits", `{}`))

	endpoint := map[string]any{"Address": testHost, "CachePeriodInMinutes": 1440.0}
	require.Equal(t, map[string]any{"Endpoints": []any{endpoint}}, s.ok(t, "DescribeEndpoints", `{}`))
}

// Test_emptyRequests: a request with no members is a 400, never a panic.
func Test_emptyRequests(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable(tab1))

	for _, op := range []string{opCreate, opDescribe, opDelete, opUpdate, opPut, opGet, opDeleteI} {
		got := s.call(t, usEast1, op, `{}`)
		require.Equal(t, http.StatusBadRequest, got.status, "%s: %s", op, got.body)
	}

	for _, op := range []string{opPut, opGet, opDeleteI} {
		got := s.call(t, usEast1, op, describeTab1)
		require.Equal(t, http.StatusBadRequest, got.status, "%s: %s", op, got.body)
	}
}
