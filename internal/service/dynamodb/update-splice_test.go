package dynamodb_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

// Test_updateItem_spliced: an UpdateItem of a stored item splices the
// attributes it writes into the stored item's bytes. Every reader of those
// bytes reads the item the update made: its ALL_NEW answer, a GetItem, the
// stream's new and old images, a legacy AttributeUpdates, and a table
// restored from a snapshot taken after it.
func Test_updateItem_spliced(t *testing.T) {
	s := newStack(t)
	s.ok(t, "CreateTable", fmt.Sprintf(streamTable, "NEW_AND_OLD_IMAGES"))

	big := strings.Repeat("z", 4<<10)
	item := `{"id":{"S":"k"},"a\"b":{"N":"1"},"m":{"M":{"x":{"S":"1"},"y":{"L":[{"N":"2"}]}}},` +
		`"s":{"SS":["p","q"]},"big":{"S":"` + big + `"}}`
	s.ok(t, opPut, `{"TableName":"streamed","Item":`+item+`}`)

	steps := []struct {
		name, update, want string
	}{
		{
			name: "set an escaped name",
			update: `"UpdateExpression":"SET #q = :n","ExpressionAttributeNames":{"#q":"a\"b"},` +
				`"ExpressionAttributeValues":{":n":{"N":"2"}}`,
			want: `{"id":{"S":"k"},"a\"b":{"N":"2"},"m":{"M":{"x":{"S":"1"},"y":{"L":[{"N":"2"}]}}},` +
				`"s":{"SS":["p","q"]},"big":{"S":"` + big + `"}}`,
		},
		{
			name: "remove one attribute and below another, add to a set",
			update: `"UpdateExpression":"REMOVE big, m.x ADD s :s",` +
				`"ExpressionAttributeValues":{":s":{"SS":["r"]}}`,
			want: `{"id":{"S":"k"},"a\"b":{"N":"2"},"m":{"M":{"y":{"L":[{"N":"2"}]}}},"s":{"SS":["p","q","r"]}}`,
		},
		{
			name:   "copy one attribute to a new one",
			update: `"UpdateExpression":"SET n2 = m.y"`,
			want: `{"id":{"S":"k"},"a\"b":{"N":"2"},"m":{"M":{"y":{"L":[{"N":"2"}]}}},"s":{"SS":["p","q","r"]},` +
				`"n2":{"L":[{"N":"2"}]}}`,
		},
		{
			name:   "legacy AttributeUpdates",
			update: `"AttributeUpdates":{"s":{"Action":"DELETE"},"t":{"Action":"PUT","Value":{"BOOL":true}}}`,
			want: `{"id":{"S":"k"},"a\"b":{"N":"2"},"m":{"M":{"y":{"L":[{"N":"2"}]}}},` +
				`"n2":{"L":[{"N":"2"}]},"t":{"BOOL":true}}`,
		},
	}

	old := item

	for _, step := range steps {
		got := s.call(t, usEast1, "UpdateItem", `{"TableName":"streamed","Key":{"id":{"S":"k"}},`+
			step.update+`,"ReturnValues":"ALL_NEW"}`)
		require.JSONEq(t, `{"Attributes":`+step.want+`}`, got.body, step.name)

		require.JSONEq(t, `{"Item":`+step.want+`}`,
			s.call(t, usEast1, opGet, `{"TableName":"streamed","Key":{"id":{"S":"k"}}}`).body, step.name)

		records := s.records(t, "streamed")
		last := records[len(records)-1]
		require.JSONEq(t, step.want, mustJSON(t, value.Unpack(last.New)), step.name)
		require.JSONEq(t, old, mustJSON(t, value.Unpack(last.Old)), step.name)

		old = step.want
	}

	restored := newStack(t)
	require.NoError(t, restored.provider.Restore(bytes.NewReader(snapshotOf(t, s.provider))))
	require.JSONEq(t, `{"Item":`+old+`}`,
		restored.call(t, usEast1, opGet, `{"TableName":"streamed","Key":{"id":{"S":"k"}}}`).body)
}
