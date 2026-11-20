package dynamodb_test

import (
	"testing"
)

// opExecTransaction is ExecuteTransaction.
const opExecTransaction = "ExecuteTransaction"

// The messages of the Query and Scan order rows, each the LocalStack image's
// answer (gostack-parity-localstack:8b9a79f, SQ-331).
const (
	ordKeyType      = "One or more parameter values were invalid: Condition parameter type does not match schema type"
	ordKeyNotSup    = "Query key condition not supported"
	ordKeyMissed    = "Query condition missed key schema element"
	ordKeyLength    = "Conditions can be of length 1 or 2 only"
	ordKeyTwice     = "KeyConditionExpressions must only contain one condition per key"
	ordKeyOperator  = "Invalid operator used in KeyConditionExpression: OR"
	ordStartType    = "Type mismatch for attribute to update"
	ordStartBad     = "The provided starting key is invalid"
	ordStartSize    = "Exclusive Start Key must have same size as table's key schema"
	ordConsistent   = "Consistent reads are not supported on global secondary indexes"
	ordLimit        = "Limit must be greater than or equal to 1"
	ordSelectBad    = "Select must be ALL_ATTRIBUTES, ALL_PROJECTED_ATTRIBUTES, COUNT, or SPECIFIC_ATTRIBUTES"
	ordSelectCount  = "Cannot specify the ProjectionExpression when choosing to get only the Count"
	ordSegmentAlone = "If segment is specified, total segment must also be specified"
	ordQueryValues  = "ExpressionAttributeValues can only be specified when using expressions: " +
		"FilterExpression and KeyConditionExpression are null"
	ordScanValues = "ExpressionAttributeValues can only be specified when using expressions: FilterExpression is null"
	ordUnusedVal  = "Value provided in ExpressionAttributeValues unused in expressions: keys: {:u}"
	ordNoKeyCond  = "Either the KeyConditions or KeyConditionExpression parameter must be specified in the request."
	ordFilterSyn  = `Invalid FilterExpression: Syntax error; token: "<EOF>", near: "= "`
	ordProjSyn    = `Invalid ProjectionExpression: Syntax error; token: "=", near: "x = "`
	ordScanGSI    = "One or more parameter values were invalid: Select type ALL_ATTRIBUTES is not supported " +
		"for global secondary index gkk because its projection type is not ALL"
	ordTwice = "Cannot have two attributes with the same name"
)

// orderTables is a table keyed by the strings pk and sk with a keys-only global
// index gkk on g and the number n and a keys-only local index lkk on n, and a
// table keyed by pk alone.
const (
	orderTable = `{"TableName":"hrt","BillingMode":"PAY_PER_REQUEST",` +
		`"KeySchema":[{"AttributeName":"pk","KeyType":"HASH"},{"AttributeName":"sk","KeyType":"RANGE"}],` +
		`"AttributeDefinitions":[{"AttributeName":"pk","AttributeType":"S"},{"AttributeName":"sk","AttributeType":"S"},` +
		`{"AttributeName":"g","AttributeType":"S"},{"AttributeName":"n","AttributeType":"N"}],` +
		`"GlobalSecondaryIndexes":[{"IndexName":"gkk","KeySchema":[{"AttributeName":"g","KeyType":"HASH"},` +
		`{"AttributeName":"n","KeyType":"RANGE"}],"Projection":{"ProjectionType":"KEYS_ONLY"}}],` +
		`"LocalSecondaryIndexes":[{"IndexName":"lkk","KeySchema":[{"AttributeName":"pk","KeyType":"HASH"},` +
		`{"AttributeName":"n","KeyType":"RANGE"}],"Projection":{"ProjectionType":"KEYS_ONLY"}}]}`
	orderHashTable = `{"TableName":"htt","BillingMode":"PAY_PER_REQUEST",` +
		`"KeySchema":[{"AttributeName":"pk","KeyType":"HASH"}],` +
		`"AttributeDefinitions":[{"AttributeName":"pk","AttributeType":"S"}]}`

	// valuesAB are the placeholders of the rows: :a and :b strings, :n a
	// number, :e an empty string, :u one no expression uses.
	valuesA = `"ExpressionAttributeValues":{":a":{"S":"a"}`
	valuesE = valuesA + `,":e":{"S":""}`
	valuesN = valuesA + `,":n":{"N":"1"}`

	valuesOnlyE = `"ExpressionAttributeValues":{":e":{"S":""}`
	valuesOnlyN = `"ExpressionAttributeValues":{":n":{"N":"1"}`
)

// Test_readChecks_imageOrder pins the order a Query and a Scan make their
// checks in, and the checks a key condition and an exclusive start key get:
// every row is a request with two problems, answered with the one the image
// reports first.
func Test_readChecks_imageOrder(t *testing.T) {
	t.Parallel()

	s := newStack(t)
	s.ok(t, "CreateTable", orderTable)
	s.ok(t, "CreateTable", orderHashTable)

	const (
		eskGSI   = `"ExclusiveStartKey":{"g":{"S":"x"},"n":{"N":"1"},"pk":{"S":"a"},"sk":{"S":"b"}}`
		eskTable = `"ExclusiveStartKey":{"pk":{"N":"1"},"sk":{"S":"b"}}`
		gsiHead  = `{"TableName":"hrt","IndexName":"gkk","KeyConditionExpression":"g = :a",`
		tblHead  = `{"TableName":"hrt","KeyConditionExpression":"pk = :a",`
	)

	for _, c := range []struct{ name, op, body, code, message string }{
		// A key condition with two problems.
		{
			"key type before missed range", opQuery,
			`{"TableName":"hrt","KeyConditionExpression":"pk = :n AND zz = :a",` + valuesN + `}}`,
			codeValidation, ordKeyType,
		},
		{
			"key operator before range type", opQuery,
			`{"TableName":"hrt","KeyConditionExpression":"pk > :a AND sk = :n",` + valuesN + `}}`,
			codeValidation, ordKeyNotSup,
		},
		{
			"hash type before operator", opQuery,
			`{"TableName":"hrt","KeyConditionExpression":"pk > :n",` + valuesOnlyN + `}}`, codeValidation, ordKeyType,
		},
		{
			"hash operator before empty", opQuery,
			`{"TableName":"hrt","KeyConditionExpression":"pk > :e",` + valuesOnlyE + `}}`, codeValidation, ordKeyNotSup,
		},
		{
			"hash empty before range type", opQuery,
			`{"TableName":"hrt","KeyConditionExpression":"sk = :n AND pk = :e",` + valuesOnlyE + `,":n":{"N":"1"}}}`,
			codeValidation,
			emptyStr + "pk",
		},
		{
			"hash empty before missed range", opQuery,
			`{"TableName":"hrt","KeyConditionExpression":"pk = :e AND zz = :a",` + valuesE + `}}`,
			codeValidation, emptyStr + "pk",
		},
		{
			"missed hash alone", opQuery,
			`{"TableName":"hrt","KeyConditionExpression":"zz = :a",` + valuesA + `}}`, codeValidation, ordKeyMissed,
		},
		{
			"hash-only table with two conditions", opQuery,
			`{"TableName":"htt","KeyConditionExpression":"pk = :a AND zz = :n",` + valuesN + `}}`,
			codeValidation, ordKeyNotSup,
		},
		{
			"three conditions", opQuery,
			`{"TableName":"htt","KeyConditionExpression":"pk = :a AND sk = :a AND zz = :a",` + valuesA + `}}`,
			codeValidation,
			ordKeyLength,
		},
		{
			"one condition per key before length", opQuery,
			`{"TableName":"hrt","KeyConditionExpression":"pk = :a AND pk = :a AND sk = :a",` + valuesA + `}}`,
			codeValidation,
			ordKeyTwice,
		},

		// An exclusive start key.
		{
			"start key type before range predicate", opQuery, tblHead + eskTable + `,` + valuesA + `}}`,
			codeValidation, ordStartType,
		},
		{"start key empty", opQuery, tblHead + `"ExclusiveStartKey":{},` + valuesA + `}}`, codeValidation, ordStartBad},
		{
			"start key one name short on a table", opQuery,
			tblHead + `"ExclusiveStartKey":{"pk":{"S":"a"},"zz":{"S":"b"}},` + valuesA + `}}`,
			codeValidation, ordStartBad,
		},
		{
			"start key type before the missing name", opQuery,
			tblHead + `"ExclusiveStartKey":{"pk":{"N":"1"},"zz":{"S":"b"}},` + valuesA + `}}`,
			codeValidation, ordStartType,
		},
		{
			"start key wrong size on a global index", opQuery,
			gsiHead + `"ExclusiveStartKey":{"g":{"S":"x"}},` + valuesA + `}}`,
			codeValidation, ordStartBad,
		},
		{
			"start key wrong size on a table", opQuery,
			tblHead + `"ExclusiveStartKey":{"pk":{"S":"a"}},` + valuesA + `}}`,
			codeValidation, ordStartSize,
		},
		{"scan start key type", opScan, `{"TableName":"hrt",` + eskTable + `}`, codeValidation, ordStartType},
		{
			"scan start key size on a global index", opScan,
			`{"TableName":"hrt","IndexName":"gkk","ExclusiveStartKey":{"g":{"S":"x"}}}`,
			codeValidation, ordStartBad,
		},
		{
			"scan start key before select", opScan, `{"TableName":"hrt","Select":"BAD",` + eskTable + `}`,
			codeValidation,
			ordStartType,
		},
		{
			"query start key before select", opQuery, tblHead + `"Select":"BAD",` + eskTable + `,` + valuesA + `}}`,
			codeValidation,
			ordStartType,
		},

		// The order of a Query's checks.
		{
			"query key condition before select", opQuery,
			`{"TableName":"hrt","KeyConditionExpression":"pk = :e","Select":"COUNT","ProjectionExpression":"x",` +
				valuesOnlyE + `}}`,
			codeValidation, emptyStr + "pk",
		},
		{
			"query select last", opQuery, tblHead + `"Select":"BAD","FilterExpression":"sk = :a",` + valuesA + `}}`,
			codeValidation,
			"Filter Expression can only contain non-primary key attributes: Primary key attribute: sk",
		},
		{
			"query projection before select", opQuery,
			gsiHead + `"Select":"ALL_PROJECTED_ATTRIBUTES","ProjectionExpression":"x = ",` + valuesA + `}}`,
			codeValidation,
			ordProjSyn,
		},
		{
			"query filter before missing key condition", opQuery, `{"TableName":"hrt","FilterExpression":"x = "}`,
			codeValidation,
			ordFilterSyn,
		},
		{
			"query values before missing key condition", opQuery,
			`{"TableName":"hrt","ExpressionAttributeValues":{":u":{"S":"u"}}}`, codeValidation, ordQueryValues,
		},
		{"query missing key condition", opQuery, `{"TableName":"hrt","Select":"BAD"}`, codeValidation, ordNoKeyCond},
		{
			"query unused before consistent", opQuery,
			gsiHead + `"ConsistentRead":true,` + valuesA + `,":u":{"S":"u"}}}`, codeValidation, ordUnusedVal,
		},
		{
			"query consistent before key type", opQuery,
			`{"TableName":"hrt","IndexName":"gkk","KeyConditionExpression":"g = :n","ConsistentRead":true,` + valuesOnlyN + `}}`,
			codeValidation, ordConsistent,
		},
		{
			"query operator before consistent", opQuery,
			`{"TableName":"hrt","IndexName":"gkk","KeyConditionExpression":"g = :a OR n = :n","ConsistentRead":true,` +
				valuesN + `}}`, codeValidation, ordKeyOperator,
		},
		{
			"query one condition per key before consistent", opQuery,
			`{"TableName":"hrt","IndexName":"gkk","KeyConditionExpression":"g = :a AND g = :a","ConsistentRead":true,` +
				valuesA + `}}`, codeValidation, ordKeyTwice,
		},

		// The order of a Scan's checks.
		{
			"scan consistent before limit", opScan,
			`{"TableName":"hrt","IndexName":"gkk","ConsistentRead":true,"Limit":0}`,
			codeValidation, ordConsistent,
		},
		{
			"scan values before limit", opScan,
			`{"TableName":"hrt","Limit":0,"ExpressionAttributeValues":{":u":{"S":"u"}}}`,
			codeValidation, ordScanValues,
		},
		{
			"scan limit before filter", opScan, `{"TableName":"hrt","Limit":0,"FilterExpression":"x = "}`,
			codeValidation, ordLimit,
		},
		{
			"scan filter before select", opScan, `{"TableName":"hrt","Select":"BAD","FilterExpression":"x = "}`,
			codeValidation,
			ordFilterSyn,
		},
		{
			"scan projection before select", opScan,
			`{"TableName":"hrt","Select":"COUNT","ProjectionExpression":"x = "}`,
			codeValidation, ordProjSyn,
		},
		{
			"scan select before segments", opScan, `{"TableName":"hrt","Select":"BAD","Segment":0}`,
			codeValidation, ordSelectBad,
		},
		{
			"scan start key before segments", opScan, `{"TableName":"hrt",` + eskTable + `,"Segment":0}`,
			codeValidation,
			ordStartType,
		},
		{
			"scan segments before attributes", opScan, `{"TableName":"hrt","AttributesToGet":["x","x"],"Segment":0}`,
			codeValidation, ordSegmentAlone,
		},
		{
			"scan select before attributes", opScan, `{"TableName":"hrt","AttributesToGet":["x","x"],"Select":"COUNT"}`,
			codeValidation, "Cannot specify the AttributesToGet when choosing to get only the Count",
		},
		{
			"scan start key before attributes", opScan,
			`{"TableName":"hrt","AttributesToGet":["x","x"],` + eskTable + `}`,
			codeValidation, ordStartType,
		},
		{"scan attributes", opScan, `{"TableName":"hrt","AttributesToGet":["x","x"]}`, codeValidation, ordTwice},
		{
			"scan select all on a keys-only global index", opScan,
			`{"TableName":"hrt","IndexName":"gkk","Select":"ALL_ATTRIBUTES"}`,
			codeValidation, ordScanGSI,
		},
		{
			"scan projection before select all on a global index", opScan,
			`{"TableName":"hrt","IndexName":"gkk","Select":"ALL_ATTRIBUTES","ProjectionExpression":"x = "}`,
			codeValidation,
			ordProjSyn,
		},
		{
			"scan select count with projection before segments", opScan,
			`{"TableName":"hrt","Select":"COUNT","ProjectionExpression":"x","Segment":0}`,
			codeValidation, ordSelectCount,
		},

		// A statement's ReturnValuesOnConditionCheckFailure in ExecuteTransaction.
		{
			"transaction enum last", opExecTransaction,
			`{"TransactStatements":[{"Statement":"UPDATE \"hrt\" SET x=1 WHERE pk='a' AND sk='b'",` +
				`"ReturnValuesOnConditionCheckFailure":"BAD"}]}`, codeValidation,
			"Validation failed in TransactStatements[0]: No enum constant " +
				"software.amazon.awssdk.services.dynamodb.model.ReturnValuesOnConditionCheckFailure.BAD",
		},
		{
			"transaction syntax before enum", opExecTransaction,
			`{"TransactStatements":[{"Statement":"UPDATE hrt SET WHERE","ReturnValuesOnConditionCheckFailure":"BAD"}]}`,
			codeValidation, "Statement wasn't well formed, can't be processed: UPDATE hrt SET WHERE",
		},
		{
			"transaction missing table before enum", opExecTransaction,
			`{"TransactStatements":[{"Statement":"UPDATE \"nope\" SET x=1 WHERE pk='a' AND sk='b'",` +
				`"ReturnValuesOnConditionCheckFailure":"BAD"}]}`, "ResourceNotFoundException", "Requested resource not found",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			s.fails(t, c.op, c.body, c.code, c.message)
		})
	}
}
