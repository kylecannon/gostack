package dynamodb_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/kylecannon/gostack/internal/service/dynamodb/stream"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

// ownedTable streams both images and indexes g, so an update reaches every
// reader of the old item.
const ownedTable = `{"TableName":"owned","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],` +
	`"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"},{"AttributeName":"g","AttributeType":"S"}],` +
	`"GlobalSecondaryIndexes":[{"IndexName":"byg","KeySchema":[{"AttributeName":"g","KeyType":"HASH"}],` +
	`"Projection":{"ProjectionType":"ALL"}}],"BillingMode":"PAY_PER_REQUEST",` +
	`"StreamSpecification":{"StreamEnabled":true,"StreamViewType":"NEW_AND_OLD_IMAGES"}}`

// ownedItem is the item each case starts from: n and m.x are the attributes
// the updates change and the readers read back.
const ownedItem = `{"id":{"S":"a"},"n":{"N":"1"},"m":{"M":{"x":{"N":"1"}}},"g":{"S":"G1"}}`

func newOwned(t *testing.T) *stack {
	t.Helper()

	s := newStack(t)
	s.ok(t, opCreate, ownedTable)
	s.ok(t, opPut, `{"TableName":"owned","Item":`+ownedItem+`}`)

	return s
}

// ownedStored is the item stored under id a.
func ownedStored(t *testing.T, s *stack) string {
	t.Helper()

	got := s.call(t, usEast1, opGet, `{"TableName":"owned","Key":{"id":{"S":"a"}}}`)
	require.Equal(t, http.StatusOK, got.status, got.body)

	var out map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(got.body), &out))

	return string(out["Item"])
}

// ownedSet is an UpdateItem of id a that sets n and m.x to v, with extra
// members appended to the request.
func ownedSet(v, extra string) string {
	return `{"TableName":"owned","Key":{"id":{"S":"a"}},"UpdateExpression":"SET n = :v, m.x = :v",` +
		`"ExpressionAttributeValues":{":v":{"N":"` + v + `"}}` + extra + `}`
}

// Test_applyOwned_readersSeeTheOldItem: UpdateItem applies its update into the
// item it unpacked from the stored one. Each reader of the old item still reads
// it as it was before the update: the stream's old image, ReturnValues ALL_OLD
// and UPDATED_OLD, the item a failed condition returns, the item stored after
// an update that fails part way, a transaction cancelled after deciding an
// update, and the index entry under the old key.
func Test_applyOwned_readersSeeTheOldItem(t *testing.T) {
	t.Run("stream images and ALL_OLD", func(t *testing.T) {
		s := newOwned(t)

		got := s.ok(t, "UpdateItem", ownedSet("2", `,"ReturnValues":"ALL_OLD"`))
		attrs, err := json.Marshal(got["Attributes"])
		require.NoError(t, err)
		require.JSONEq(t, ownedItem, string(attrs))

		updated := `{"id":{"S":"a"},"n":{"N":"2"},"m":{"M":{"x":{"N":"2"}}},"g":{"S":"G1"}}`
		require.JSONEq(t, updated, ownedStored(t, s))

		records := s.records(t, "owned")
		require.Len(t, records, 2)
		require.Equal(t, stream.Modify, records[1].Event)
		require.JSONEq(t, ownedItem, itemJSON(t, value.Unpack(records[1].Old)), "the old image")
		require.JSONEq(t, updated, itemJSON(t, value.Unpack(records[1].New)), "the new image")
	})

	t.Run("UPDATED_OLD", func(t *testing.T) {
		s := newOwned(t)

		got := s.ok(t, "UpdateItem", ownedSet("2", `,"ReturnValues":"UPDATED_OLD"`))
		attrs, err := json.Marshal(got["Attributes"])
		require.NoError(t, err)
		require.JSONEq(t, `{"n":{"N":"1"},"m":{"M":{"x":{"N":"1"}}}}`, string(attrs))
	})

	t.Run("condition failure returns the old item", func(t *testing.T) {
		s := newOwned(t)

		got := s.call(t, usEast1, "UpdateItem", `{"TableName":"owned","Key":{"id":{"S":"a"}},`+
			`"UpdateExpression":"SET n = :v, m.x = :v","ExpressionAttributeValues":{":v":{"N":"2"},":z":{"N":"0"}},`+
			`"ConditionExpression":"n = :z","ReturnValuesOnConditionCheckFailure":"ALL_OLD"}`)
		require.Equal(t, http.StatusBadRequest, got.status, got.body)

		var out map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(got.body), &out))
		require.JSONEq(t, ownedItem, string(out["Item"]))
		require.JSONEq(t, ownedItem, ownedStored(t, s))
	})

	t.Run("an update that fails part way stores nothing", func(t *testing.T) {
		s := newOwned(t)

		// SET n is applied before m.x + missing fails, into the unpacked item.
		got := s.call(t, usEast1, "UpdateItem", `{"TableName":"owned","Key":{"id":{"S":"a"}},`+
			`"UpdateExpression":"SET n = :v, m.x = missing + :v","ExpressionAttributeValues":{":v":{"N":"2"}}}`)
		require.Equal(t, http.StatusBadRequest, got.status, got.body)
		require.JSONEq(t, ownedItem, ownedStored(t, s))
		require.Len(t, s.records(t, "owned"), 1, "no record of a write that failed")
	})

	t.Run("cancelled transaction", func(t *testing.T) {
		s := newOwned(t)

		got := s.call(t, usEast1, "TransactWriteItems", `{"TransactItems":[`+
			`{"Update":{"TableName":"owned","Key":{"id":{"S":"a"}},"UpdateExpression":"SET n = :v, m.x = :v",`+
			`"ExpressionAttributeValues":{":v":{"N":"2"}}}},`+
			`{"ConditionCheck":{"TableName":"owned","Key":{"id":{"S":"b"}},`+
			`"ConditionExpression":"attribute_exists(id)"}}]}`)
		require.Equal(t, http.StatusBadRequest, got.status, got.body)
		require.Contains(t, got.body, "TransactionCanceledException")
		require.JSONEq(t, ownedItem, ownedStored(t, s))
	})

	t.Run("index key change", func(t *testing.T) {
		s := newOwned(t)

		s.ok(t, "UpdateItem", `{"TableName":"owned","Key":{"id":{"S":"a"}},"UpdateExpression":"SET g = :g",`+
			`"ExpressionAttributeValues":{":g":{"S":"G2"}}}`)

		for g, want := range map[string]float64{"G1": 0, "G2": 1} {
			got := s.ok(t, "Query", `{"TableName":"owned","IndexName":"byg","KeyConditionExpression":"g = :g",`+
				`"ExpressionAttributeValues":{":g":{"S":"`+g+`"}}}`)
			require.InDelta(t, want, got["Count"], 0, "items under %s", g)
		}
	})
}

// itemJSON is item as the wire writes it.
func itemJSON(t *testing.T, item value.Item) string {
	t.Helper()

	b, err := json.Marshal(item)
	require.NoError(t, err)

	return string(b)
}
