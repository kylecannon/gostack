package table

import (
	"strings"
	"testing"
	"unsafe"

	api "github.com/kylecannon/gostack/internal/api/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/value"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shareTable is a table keyed by pk (S) and sk (of type typ), with a global
// index on g (S) and the same sk.
func shareTable(typ string) *Table {
	key := KeySchema{
		Hash:  KeyAttribute{Name: "pk", Type: "S"},
		Range: KeyAttribute{Name: "sk", Type: typ},
	}

	return New(key, []IndexSpec{{
		Name:       "bySk",
		Global:     true,
		Key:        KeySchema{Hash: KeyAttribute{Name: "g", Type: "S"}, Range: key.Range},
		Projection: Projection{Type: ProjectKeysOnly},
	}})
}

// shareKeys returns an item with a 1,000-byte range key of type typ, and a key
// for it that holds a copy of the range key's bytes, as a request's does.
func shareKeys(typ string) (item, key value.Item) {
	pk, g := "p", "g"
	body := strings.Repeat("k", 1000)

	var sk, skCopy api.AttributeValue

	switch typ {
	case "B":
		sk, skCopy = api.AttributeValue{B: []byte(body)}, api.AttributeValue{B: []byte(body)}
	default:
		other := strings.Clone(body)
		sk, skCopy = api.AttributeValue{S: &body}, api.AttributeValue{S: &other}
	}

	item = value.Item{"pk": {S: &pk}, "g": {S: &g}, "sk": sk}
	key = value.Item{"pk": {S: &pk}, "sk": skCopy}

	return item, key
}

// stringData is where the bytes of the string key are.
func stringData(k string) *byte { return unsafe.StringData(k) }

// itemData is where the bytes of the item's range key are.
func itemData(v *api.AttributeValue) *byte {
	if v.S != nil {
		return unsafe.StringData(*v.S)
	}

	return unsafe.SliceData(v.B)
}

// Test_Table_Write_sortKeySharesTheItemsBytes holds the entries of a string or
// binary range key, in the table and in an index, to the bytes of the stored
// item, not to a copy and not to the request's key.
func Test_Table_Write_sortKeySharesTheItemsBytes(t *testing.T) {
	for _, typ := range []string{"S", "B"} {
		tbl := shareTable(typ)
		item, key := shareKeys(typ)

		require.NoError(t, value.ValidateItem(item))

		_, err := tbl.Write(key, func(value.Item) (value.Item, error) { return item, nil })
		require.NoError(t, err)

		sk := item["sk"]

		for name, data := range map[string]*store{"table": &tbl.primary, "index": &tbl.index("bySk").data} {
			require.Len(t, data.parts, 1, "%s %s", typ, name)

			for _, p := range data.parts {
				require.Len(t, p.entries, 1, "%s %s", typ, name)
				assert.Same(t, itemData(&sk), stringData(p.entries[0].sort), "%s %s entry", typ, name)
			}
		}
	}
}

// Test_Table_Load_sortKeySharesTheItemsBytes is the same for a restored item.
func Test_Table_Load_sortKeySharesTheItemsBytes(t *testing.T) {
	for _, typ := range []string{"S", "B"} {
		tbl := shareTable(typ)
		item, _ := shareKeys(typ)

		require.NoError(t, tbl.Load([]value.Item{item}))

		sk := item["sk"]

		for name, data := range map[string]*store{"table": &tbl.primary, "index": &tbl.index("bySk").data} {
			for _, p := range data.parts {
				require.Len(t, p.entries, 1, "%s %s", typ, name)
				assert.Same(t, itemData(&sk), stringData(p.entries[0].sort), "%s %s entry", typ, name)
			}
		}
	}
}
