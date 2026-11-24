package dynamodb

import (
	"encoding/json/v2"
	"testing"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/request"
	"github.com/kylecannon/gostack/internal/service/dynamodb/table"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

// Test_undo_restoresTheItemAnUpdateWroteInto: a transaction's update applies
// into the item it unpacked, so the item undo writes back comes from the
// stored bytes. The update is decided and written, as applyTransaction does
// before a later write fails, and undo restores the item as it was, nested
// map included.
func Test_undo_restoresTheItemAnUpdateWroteInto(t *testing.T) {
	const stored = `{"id":{"S":"a"},"n":{"N":"1"},"m":{"M":{"x":{"N":"1"}}}}`

	p := New(func() Settings { return Settings{} }, nil)
	ctx := request.With(t.Context(), &request.Context{AccountID: "000000000000", Region: usEast1})

	var create api.CreateTableInput

	require.NoError(t, json.Unmarshal([]byte(`{"TableName":"tab","BillingMode":"PAY_PER_REQUEST",`+
		`"KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],`+
		`"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}]}`), &create))

	_, err := p.CreateTable(ctx, &create)
	require.NoError(t, err)

	var put api.PutItemInput

	require.NoError(t, json.Unmarshal([]byte(`{"TableName":"tab","Item":`+stored+`}`), &put))

	_, err = p.PutItem(ctx, &put)
	require.NoError(t, err)

	var tx api.TransactWriteItemsInput

	require.NoError(t, json.Unmarshal([]byte(`{"TransactItems":[{"Update":{"TableName":"tab",`+
		`"Key":{"id":{"S":"a"}},"UpdateExpression":"SET n = :v, m.x = :v",`+
		`"ExpressionAttributeValues":{":v":{"N":"2"}}}}]}`), &tx))

	actions, err := p.caller(ctx).checkTransactWrite(tx.TransactItems)
	require.NoError(t, err)
	require.Len(t, actions, 1)

	a := actions[0]
	unlock := table.Lock(a.entry.data)

	_, result, err := a.decide()
	require.NoError(t, err)
	require.True(t, result.write)

	_, err = a.entry.data.WriteHeld(a.key, func(value.Packed) (value.Item, error) { return result.new, nil })
	require.NoError(t, err)

	written, err := json.Marshal(a.entry.data.GetHeld(a.key))
	require.NoError(t, err)
	require.JSONEq(t, `{"id":{"S":"a"},"n":{"N":"2"},"m":{"M":{"x":{"N":"2"}}}}`, string(written))

	undo(actions, []txResult{result})

	restored, err := json.Marshal(a.entry.data.GetHeld(a.key))
	require.NoError(t, err)
	unlock()
	require.JSONEq(t, stored, string(restored))
}
