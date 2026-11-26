package dynamodb_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/require"
)

// splicedFillers are attributes that make an item large enough for each
// update below to pay to splice, and hold enough members that the order a
// splice leaves cannot be Pack's by chance.
func splicedFillers() string {
	fillers := make([]string, 24)
	for i := range fillers {
		fillers[i] = fmt.Sprintf(`"f%02d":{"S":"%s"}`, i, strings.Repeat("f", 1<<10))
	}

	return strings.Join(fillers, ",")
}

// requireSpliced holds got, the stored bytes of an item an update made of
// prev, to a splice of the attributes written: the members prev holds that
// written does not list come first, in prev's order, which leaves the key
// where it was; the written ones the item holds follow; and the header is the
// one Pack writes for the item.
func requireSpliced(t *testing.T, prev, got value.Packed, written []string, what string) {
	t.Helper()

	kept := slices.DeleteFunc(packedMembers(t, prev), func(name string) bool { return slices.Contains(written, name) })
	members := packedMembers(t, got)

	require.GreaterOrEqual(t, len(members), len(kept), what)
	require.Equal(t, kept, members[:len(kept)], "the members the update left, in the stored order: %s", what)

	for _, name := range members[len(kept):] {
		require.Contains(t, written, name, what)
	}

	packed, err := value.Pack(value.Unpack(got))
	require.NoError(t, err)
	require.Equal(t, string(packed)[:len(packed)-len(packed.JSON())], string(got)[:len(got)-len(got.JSON())],
		"header (size, scalars, attributes): %s", what)
}

// packedMembers is the names of p's top-level members, in its json's order.
func packedMembers(t *testing.T, p value.Packed) []string {
	t.Helper()

	dec := json.NewDecoder(strings.NewReader(p.JSON()))

	_, err := dec.Token()
	require.NoError(t, err)

	var names []string

	for dec.More() {
		tok, err := dec.Token()
		require.NoError(t, err)

		var raw json.RawMessage
		require.NoError(t, dec.Decode(&raw))

		names = append(names, tok.(string)) //nolint:forcetypeassert // a member's name is a string
	}

	return names
}

// Test_updateItem_spliced: an UpdateItem of a stored item splices the
// attributes it writes into the stored item's bytes, which the stream's new
// image is. Every reader of those bytes reads the item the update made: its
// ALL_NEW answer, a GetItem, the stream's new and old images, a legacy
// AttributeUpdates, and a table restored from a snapshot taken after it.
func Test_updateItem_spliced(t *testing.T) {
	s := newStack(t)
	s.ok(t, "CreateTable", fmt.Sprintf(streamTable, "NEW_AND_OLD_IMAGES"))

	big := strings.Repeat("z", 4<<10)
	fillers := splicedFillers()
	item := `{"id":{"S":"k"},"a\"b":{"N":"1"},"m":{"M":{"x":{"S":"1"},"y":{"L":[{"N":"2"}]}}},` +
		`"s":{"SS":["p","q"]},"big":{"S":"` + big + `"},` + fillers + `}`
	s.ok(t, opPut, `{"TableName":"streamed","Item":`+item+`}`)

	steps := []struct {
		name, update, want string
		written            []string
	}{
		{
			name: "set an escaped name",
			update: `"UpdateExpression":"SET #q = :n","ExpressionAttributeNames":{"#q":"a\"b"},` +
				`"ExpressionAttributeValues":{":n":{"N":"2"}}`,
			want: `{"id":{"S":"k"},"a\"b":{"N":"2"},"m":{"M":{"x":{"S":"1"},"y":{"L":[{"N":"2"}]}}},` +
				`"s":{"SS":["p","q"]},"big":{"S":"` + big + `"},` + fillers + `}`,
			written: []string{`a"b`},
		},
		{
			name: "remove one attribute and below another, add to a set",
			update: `"UpdateExpression":"REMOVE big, m.x ADD s :s",` +
				`"ExpressionAttributeValues":{":s":{"SS":["r"]}}`,
			want: `{"id":{"S":"k"},"a\"b":{"N":"2"},"m":{"M":{"y":{"L":[{"N":"2"}]}}},"s":{"SS":["p","q","r"]},` +
				fillers + `}`,
			written: []string{"big", "m", "s"},
		},
		{
			name:   "copy one attribute to a new one",
			update: `"UpdateExpression":"SET n2 = m.y"`,
			want: `{"id":{"S":"k"},"a\"b":{"N":"2"},"m":{"M":{"y":{"L":[{"N":"2"}]}}},"s":{"SS":["p","q","r"]},` +
				`"n2":{"L":[{"N":"2"}]},` + fillers + `}`,
			written: []string{"n2"},
		},
		{
			name:   "legacy AttributeUpdates",
			update: `"AttributeUpdates":{"s":{"Action":"DELETE"},"t":{"Action":"PUT","Value":{"BOOL":true}}}`,
			want: `{"id":{"S":"k"},"a\"b":{"N":"2"},"m":{"M":{"y":{"L":[{"N":"2"}]}}},` +
				`"n2":{"L":[{"N":"2"}]},"t":{"BOOL":true},` + fillers + `}`,
			written: []string{"s", "t"},
		},
	}

	old := item
	prev := s.records(t, "streamed")[0].New

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
		require.Equal(t, prev, last.Old, step.name)
		requireSpliced(t, prev, last.New, step.written, step.name)

		old, prev = step.want, last.New
	}

	restored := newStack(t)
	require.NoError(t, restored.provider.Restore(bytes.NewReader(snapshotOf(t, s.provider))))
	require.JSONEq(t, `{"Item":`+old+`}`,
		restored.call(t, usEast1, opGet, `{"TableName":"streamed","Key":{"id":{"S":"k"}}}`).body)
}

// splicedIndexed is a table keyed by a hash and a range key, with a global
// secondary index on g and a stream that shows the stored bytes.
const splicedIndexed = `{"TableName":"ranged","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},` +
	`{"AttributeName":"r","KeyType":"RANGE"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"},` +
	`{"AttributeName":"r","AttributeType":"N"},{"AttributeName":"g","AttributeType":"S"}],` +
	`"GlobalSecondaryIndexes":[{"IndexName":"byg","KeySchema":[{"AttributeName":"g","KeyType":"HASH"}],` +
	`"Projection":{"ProjectionType":"ALL"}}],"BillingMode":"PAY_PER_REQUEST",` +
	`"StreamSpecification":{"StreamEnabled":true,"StreamViewType":"NEW_AND_OLD_IMAGES"}}`

// Test_updateItem_splicedIndexed: a splice into an item of a table with a
// range key and an index keeps both key attributes where they were, and
// moves the item in the index whose key it wrote: a Query of the table finds
// the item made, a Query of the index finds it under the new value and not
// the old one.
func Test_updateItem_splicedIndexed(t *testing.T) {
	s := newStack(t)
	s.ok(t, "CreateTable", splicedIndexed)

	fillers := splicedFillers()
	for r, g := range []string{"G1", "G9"} {
		s.ok(t, opPut, fmt.Sprintf(`{"TableName":"ranged","Item":{"id":{"S":"k"},"r":{"N":"%d"},"g":{"S":%q},`+
			`"n":{"N":"1"},%s}}`, r+1, g, fillers))
	}

	prev := s.records(t, "ranged")[0].New

	s.ok(t, opUpdateItem, `{"TableName":"ranged","Key":{"id":{"S":"k"},"r":{"N":"1"}},`+
		`"UpdateExpression":"SET g = :g ADD n :n","ExpressionAttributeValues":{":g":{"S":"G2"},":n":{"N":"1"}}}`)

	records := s.records(t, "ranged")
	requireSpliced(t, prev, records[len(records)-1].New, []string{"g", "n"}, "update of the index key")

	want := `{"id":{"S":"k"},"r":{"N":"1"},"g":{"S":"G2"},"n":{"N":"2"},` + fillers + `}`

	require.JSONEq(t, `{"Items":[`+want+`],"Count":1,"ScannedCount":1}`,
		s.call(t, usEast1, opQuery, `{"TableName":"ranged","KeyConditionExpression":"id = :id AND r = :r",`+
			`"ExpressionAttributeValues":{":id":{"S":"k"},":r":{"N":"1"}}}`).body)

	require.JSONEq(t, `{"Items":[`+want+`],"Count":1,"ScannedCount":1}`,
		s.call(t, usEast1, opQuery, `{"TableName":"ranged","IndexName":"byg","KeyConditionExpression":"g = :g",`+
			`"ExpressionAttributeValues":{":g":{"S":"G2"}}}`).body)

	require.JSONEq(t, `{"Items":[],"Count":0,"ScannedCount":0}`,
		s.call(t, usEast1, opQuery, `{"TableName":"ranged","IndexName":"byg","KeyConditionExpression":"g = :g",`+
			`"ExpressionAttributeValues":{":g":{"S":"G1"}}}`).body)
}
