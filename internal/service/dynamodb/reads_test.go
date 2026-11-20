package dynamodb_test

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	opQuery = "Query"
	opScan  = "Scan"
)

// rangeTable is a CreateTable body for a table keyed by pk and the number sk,
// with a global index keyed by g, keys only, and a local one keyed by l
// holding x.
const rangeTable = `{"TableName":"tab2","BillingMode":"PAY_PER_REQUEST",` +
	`"KeySchema":[{"AttributeName":"pk","KeyType":"HASH"},{"AttributeName":"sk","KeyType":"RANGE"}],` +
	`"AttributeDefinitions":[{"AttributeName":"pk","AttributeType":"S"},{"AttributeName":"sk","AttributeType":"N"},` +
	`{"AttributeName":"g","AttributeType":"S"},{"AttributeName":"l","AttributeType":"S"}],` +
	`"GlobalSecondaryIndexes":[{"IndexName":"gkey","KeySchema":[{"AttributeName":"g","KeyType":"HASH"}],` +
	`"Projection":{"ProjectionType":"KEYS_ONLY"}}],` +
	`"LocalSecondaryIndexes":[{"IndexName":"lidx","KeySchema":[{"AttributeName":"pk","KeyType":"HASH"},` +
	`{"AttributeName":"l","KeyType":"RANGE"}],"Projection":{"ProjectionType":"INCLUDE","NonKeyAttributes":["x"]}}]}`

// fill puts n items in partition "a" of tab2, each with pad bytes of data.
func fill(t *testing.T, s *stack, n, pad int) {
	t.Helper()

	data := strings.Repeat("d", pad)
	for i := range n {
		s.ok(t, opPut, `{"TableName":"tab2","Item":{"pk":{"S":"a"},"sk":{"N":"`+strconv.Itoa(i)+`"},`+
			`"g":{"S":"G"},"l":{"S":"L`+strconv.Itoa(i)+`"},"x":{"N":"`+strconv.Itoa(i)+`"},"y":{"S":"`+data+`"}}}`)
	}
}

func query(extra string) string {
	return `{"TableName":"tab2","KeyConditionExpression":"pk = :a","ExpressionAttributeValues":{":a":{"S":"a"}` + extra
}

// Test_reads_pagination: a page ends at Limit, counted before the filter,
// with LastEvaluatedKey even when nothing follows; it is absent only when the
// read ran off the end.
func Test_reads_pagination(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, rangeTable)
	fill(t, s, 25, 1)

	for _, tt := range []struct {
		limit       int
		count       float64
		lastKey     bool
		lastSortKey string
	}{
		{10, 10, true, "9"},
		{25, 25, true, "24"},
		{26, 25, false, ""},
	} {
		got := s.ok(t, opQuery, query(`},"Limit":`+strconv.Itoa(tt.limit)+`}`))
		require.InDelta(t, tt.count, path(t, got, "Count"), 0)
		require.InDelta(t, tt.count, path(t, got, "ScannedCount"), 0)
		require.Len(t, path(t, got, "Items"), int(tt.count))

		if tt.lastKey {
			require.Equal(t, map[string]any{"pk": map[string]any{"S": "a"}, "sk": map[string]any{"N": tt.lastSortKey}},
				path(t, got, "LastEvaluatedKey"))
		} else {
			require.NotContains(t, got, "LastEvaluatedKey")
		}

		scan := s.ok(t, opScan, `{"TableName":"tab2","Limit":`+strconv.Itoa(tt.limit)+`}`)
		require.InDelta(t, tt.count, path(t, scan, "Count"), 0)
		require.Equal(t, tt.lastKey, scan["LastEvaluatedKey"] != nil)
	}

	// Following LastEvaluatedKey reads every item once.
	var seen int

	start := ""
	for {
		got := s.ok(t, opQuery, query(`},"Limit":10`+start+`}`))
		items, ok := path(t, got, "Items").([]any)
		require.True(t, ok)

		seen += len(items)

		last, ok := got["LastEvaluatedKey"]
		if !ok {
			break
		}

		b, err := json.Marshal(last)
		require.NoError(t, err)

		start = `,"ExclusiveStartKey":` + string(b)
	}

	require.Equal(t, 25, seen)

	none := s.ok(t, opQuery, query(`,":n":{"N":"100"}},"FilterExpression":"x > :n","Limit":3}`))
	require.InDelta(t, 0, path(t, none, "Count"), 0)
	require.InDelta(t, 3, path(t, none, "ScannedCount"), 0)
	require.Equal(t, []any{}, path(t, none, "Items"))
	require.NotNil(t, none["LastEvaluatedKey"])

	all := s.ok(t, opScan, `{"TableName":"tab2","FilterExpression":"x > :n",`+
		`"ExpressionAttributeValues":{":n":{"N":"100"}}}`)
	require.InDelta(t, 25, path(t, all, "ScannedCount"), 0)
	require.NotContains(t, all, "LastEvaluatedKey")
}

// Test_reads_fullPage: a page ends at the item that brings it to 1 MB, with
// LastEvaluatedKey only when another item follows.
func Test_reads_fullPage(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, rangeTable)
	fill(t, s, 8, 300000)

	got := s.ok(t, opQuery, query(`},"Select":"COUNT","ReturnConsumedCapacity":"TOTAL"}`))
	require.InDelta(t, 4, path(t, got, "Count"), 0)
	require.Equal(t, map[string]any{"pk": map[string]any{"S": "a"}, "sk": map[string]any{"N": "3"}},
		path(t, got, "LastEvaluatedKey"))
	require.NotContains(t, got, "Items")

	rest := s.ok(t, opScan, `{"TableName":"tab2","Select":"COUNT","ExclusiveStartKey":{"pk":{"S":"a"},"sk":{"N":"3"}}}`)
	require.InDelta(t, 4, path(t, rest, "Count"), 0)
	require.NotContains(t, rest, "LastEvaluatedKey")
}

// Test_reads_indexes: an index read projects, keys its LastEvaluatedKey on the
// index and the table, and refuses what DynamoDB refuses.
func Test_reads_indexes(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, rangeTable)
	fill(t, s, 3, 1)

	got := s.ok(t, opQuery, `{"TableName":"tab2","IndexName":"gkey","KeyConditionExpression":"g = :g",`+
		`"ExpressionAttributeValues":{":g":{"S":"G"}},"Limit":1,"ReturnConsumedCapacity":"INDEXES"}`)
	require.ElementsMatch(t, []string{"pk", "sk", "g"}, keys(t, path(t, got, "Items", 0)))
	require.ElementsMatch(t, []string{"pk", "sk", "g"}, keys(t, path(t, got, "LastEvaluatedKey")))
	require.InDelta(t, 0.5, path(t, got, "ConsumedCapacity", "GlobalSecondaryIndexes", "gkey", "CapacityUnits"), 0)

	lsi := s.ok(t, opQuery, query(`},"IndexName":"lidx","ScanIndexForward":false,"Limit":1}`))
	require.ElementsMatch(t, []string{"pk", "sk", "l", "x"}, keys(t, path(t, lsi, "Items", 0)))
	require.Equal(t, "L2", path(t, lsi, "Items", 0, "l", "S"))

	fetched := s.ok(t, opQuery, query(`},"IndexName":"lidx","Select":"ALL_ATTRIBUTES","Limit":1}`))
	require.Contains(t, path(t, fetched, "Items", 0), "y")

	for _, tt := range []struct{ body, code, message string }{
		{
			`{"TableName":"tab2","IndexName":"gkey","KeyConditionExpression":"g = :g",` +
				`"ExpressionAttributeValues":{":g":{"S":"G"}},"Select":"ALL_ATTRIBUTES"}`, codeValidation,
			"One or more parameter values were invalid: Select type ALL_ATTRIBUTES is not supported for global " +
				"secondary index gkey because its projection type is not ALL",
		},
		{
			`{"TableName":"tab2","IndexName":"gkey","KeyConditionExpression":"g = :g",` +
				`"ExpressionAttributeValues":{":g":{"S":"G"}},"ConsistentRead":true}`, codeValidation,
			"Consistent reads are not supported on global secondary indexes",
		},
		{query(`},"IndexName":"nope"}`), codeNotFound, "Index not found"},
		{
			query(`},"Select":"ALL_PROJECTED_ATTRIBUTES"}`), codeValidation,
			"ALL_PROJECTED_ATTRIBUTES can be used only when Querying using an IndexName",
		},
		{
			query(`},"Select":"COUNT","ProjectionExpression":"x"}`), codeValidation,
			"Cannot specify the ProjectionExpression when choosing to get only the Count",
		},
		{
			query(`},"Select":"SPECIFIC_ATTRIBUTES"}`), codeValidation,
			"Must specify the AttributesToGet or ProjectionExpression when choosing to get SPECIFIC_ATTRIBUTES",
		},
		{query(`},"Limit":0}`), codeValidation, "Limit must be greater than or equal to 1"},
		{
			query(`,":n":{"N":"1"}},"FilterExpression":"sk > :n"}`), codeValidation,
			"Filter Expression can only contain non-primary key attributes: Primary key attribute: sk",
		},
		{
			query(`},"ExclusiveStartKey":{"pk":{"S":"a"}}}`), codeValidation,
			"Exclusive Start Key must have same size as table's key schema",
		},
		{
			`{"TableName":"tab2"}`, codeValidation,
			"Either the KeyConditions or KeyConditionExpression parameter must be specified in the request.",
		},
	} {
		s.fails(t, opQuery, tt.body, tt.code, tt.message)
	}

	s.fails(t, opScan, `{"TableName":"tab2","Segment":3,"TotalSegments":2}`, codeValidation,
		"1 validation error detected: Value '3' at 'segment' failed to satisfy constraint: "+
			"Member must have value less than or equal to 1")
	s.fails(t, opScan, `{"TableName":"tab2","Segment":0}`, codeValidation,
		"If segment is specified, total segment must also be specified")
}

// Test_reads_segments: the segments of a parallel Scan are disjoint and
// together hold every item.
// Test_reads_startKeyPredicate: an ExclusiveStartKey must meet the whole key
// condition, sort predicate included, or the Query is refused (probed on the
// LocalStack image, SQ-224 probe4).
func Test_reads_startKeyPredicate(t *testing.T) {
	t.Parallel()

	s := newStack(t)
	s.ok(t, "CreateTable", rangeTable)
	fill(t, s, 1, 0)

	const (
		refused = "The provided starting key does not match the range key predicate"
		five    = `,":s":{"N":"5"}`
	)

	start := func(pk, sk string) string {
		return `"ExclusiveStartKey":{"pk":{"S":"` + pk + `"},"sk":{"N":"` + sk + `"}}`
	}

	for _, tt := range []struct {
		name, cond, values, start string
		ok                        bool
	}{
		{"other partition", "pk = :a", ``, start("zz", "1"), false},
		{"below greater-than", "pk = :a AND sk > :s", five, start("a", "1"), false},
		{"at greater-than", "pk = :a AND sk > :s", five, start("a", "5"), false},
		{"at greater-or-equal", "pk = :a AND sk >= :s", five, start("a", "5"), true},
		{"above less-than", "pk = :a AND sk < :s", five, start("a", "9"), false},
		{"not equal", "pk = :a AND sk = :s", five, start("a", "9"), false},
		{"outside between", "pk = :a AND sk BETWEEN :s AND :t", `,":s":{"N":"5"},":t":{"N":"7"}`, start("a", "9"), false},
		{"inside between", "pk = :a AND sk BETWEEN :s AND :t", `,":s":{"N":"5"},":t":{"N":"7"}`, start("a", "6"), true},
	} {
		body := `{"TableName":"tab2","KeyConditionExpression":"` + tt.cond + `",` +
			`"ExpressionAttributeValues":{":a":{"S":"a"}` + tt.values + `},` + tt.start + `}`
		if tt.ok {
			s.ok(t, opQuery, body)

			continue
		}

		s.fails(t, opQuery, body, codeValidation, refused)
	}

	s.fails(t, opQuery, `{"TableName":"tab2","IndexName":"lidx",`+
		`"KeyConditionExpression":"pk = :a AND begins_with(l, :s)",`+
		`"ExpressionAttributeValues":{":a":{"S":"a"},":s":{"S":"x"}},`+
		`"ExclusiveStartKey":{"pk":{"S":"a"},"sk":{"N":"0"},"l":{"S":"L0"}}}`, codeValidation, refused)
	s.fails(t, opQuery, `{"TableName":"tab2","IndexName":"gkey","KeyConditionExpression":"g = :g",`+
		`"ExpressionAttributeValues":{":g":{"S":"G"}},`+
		`"ExclusiveStartKey":{"pk":{"S":"a"},"sk":{"N":"0"},"g":{"S":"H"}}}`, codeValidation, refused)
}

func Test_reads_segments(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("tab1"))

	for i := range 40 {
		s.ok(t, opPut, `{"TableName":"tab1","Item":{"id":{"S":"k`+strconv.Itoa(i)+`"}}}`)
	}

	seen := map[string]int{}

	for segment := range 3 {
		got := s.ok(t, opScan, `{"TableName":"tab1","Segment":`+strconv.Itoa(segment)+`,"TotalSegments":3}`)
		items, ok := path(t, got, "Items").([]any)
		require.True(t, ok)

		for _, item := range items {
			seen[path(t, item, "id", "S").(string)]++ //nolint:forcetypeassert // a string by the line above
		}
	}

	require.Len(t, seen, 40)

	for id, n := range seen {
		require.Equal(t, 1, n, id)
	}
}

func keys(t *testing.T, v any) []string {
	t.Helper()

	m, ok := v.(map[string]any)
	require.True(t, ok)

	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	return out
}
