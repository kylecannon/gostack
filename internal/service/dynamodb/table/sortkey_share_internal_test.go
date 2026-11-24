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

		_, err := tbl.Write(key, func(value.Packed) (value.Item, error) { return item, nil })
		require.NoError(t, err)

		sk := item["sk"]
		checkShared(t, tbl, typ, itemData(&sk))
	}
}

// checkShared checks the one entry of the table and of its index bySk: a
// string range key lies in the bytes of the packed item, and a binary one, which
// the packed item holds in base64, is one copy both entries share. Neither is
// request, the bytes of the key in the item written.
func checkShared(t *testing.T, tbl *Table, typ string, request *byte) {
	t.Helper()

	var entries []entry

	for _, data := range []*store{&tbl.primary, &tbl.index("bySk").data} {
		require.Len(t, data.parts, 1, typ)

		for _, p := range data.parts {
			require.Len(t, p.entries, 1, typ)
			entries = append(entries, p.entries[0])
		}
	}

	for _, e := range entries {
		assert.NotSame(t, request, stringData(e.sort), "%s entry shares the request's bytes", typ)
	}

	if typ == "B" {
		assert.Same(t, stringData(entries[0].sort), stringData(entries[1].sort), "B entries share one copy")

		return
	}

	for _, e := range entries {
		start := uintptr(unsafe.Pointer(unsafe.StringData(string(e.item))))
		at := uintptr(unsafe.Pointer(stringData(e.sort)))
		assert.True(t, at >= start && at+uintptr(len(e.sort)) <= start+uintptr(len(e.item)), "S entry is in the packed item")
	}
}

// Test_Table_Load_sortKeySharesTheItemsBytes is the same for a restored item.
func Test_Table_Load_sortKeySharesTheItemsBytes(t *testing.T) {
	for _, typ := range []string{"S", "B"} {
		tbl := shareTable(typ)
		item, _ := shareKeys(typ)

		sk := item["sk"]
		request := itemData(&sk)

		require.NoError(t, tbl.Load([]value.Item{item}))
		checkShared(t, tbl, typ, request)
	}
}
