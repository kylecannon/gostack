package dynamodb_test

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// typesFile is a snapshot main (53c8394f) wrote, before items were packed, of
// the table typesTable with typedItems put in it.
const typesFile = "testdata/snapshot-main-types.json"

const typesTable = "types"

// typedItems are items of every attribute type, with names and strings the
// json must escape, maps nested in maps and in lists, and sets of each kind.
var typedItems = []string{
	`{"id":{"S":"scalars"},"g":{"S":"g1"},"s":{"S":"plain"},"n":{"N":"-1.50E+3"},"b":{"B":"AAEC/w=="},` +
		`"t":{"BOOL":true},"f":{"BOOL":false},"z":{"NULL":true},"e":{"S":""},"eb":{"B":""}}`,
	`{"id":{"S":"sets"},"g":{"S":"g1"},"ss":{"SS":["b","a","é"]},"ns":{"NS":["10","2.50","-3"]},` +
		`"bs":{"BS":["Ag==","AQ=="]}}`,
	`{"id":{"S":"nested"},"g":{"S":"g2"},"m":{"M":{"y":{"S":"1"},"x":{"L":[{"N":"0"},{"M":{}},{"L":[]},` +
		`{"M":{"q":{"N":"1"},"p":{"M":{"b":{"S":"2"},"a":{"NULL":true}}}}}]}}},"l":{"L":[]},"em":{"M":{}}}`,
	`{"id":{"S":"escapes \"quoted\" \\ \t\n\u0001 😀 </script>"},"a\"":{"S":"1"},"a#":{"S":"2"},` +
		`"a\\":{"S":"3"},"\u0001":{"N":"4"},"é":{"S":"tab\tnl\n"}}`,
	`{"id":{"S":"ttl"},"g":{"S":"g2"},"ttl":{"N":"99999999999"},"k9":{"S":"1"},"k10":{"S":"2"},"K":{"S":"3"}}`,
}

// putTypedItems creates typesTable, with a global index on g, and puts
// typedItems in it.
func putTypedItems(t *testing.T, s *stack) {
	t.Helper()

	s.ok(t, opCreate, `{"TableName":"`+typesTable+`","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],`+
		`"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"},{"AttributeName":"g","AttributeType":"S"}],`+
		`"BillingMode":"PAY_PER_REQUEST","GlobalSecondaryIndexes":[{"IndexName":"by-g",`+
		`"KeySchema":[{"AttributeName":"g","KeyType":"HASH"}],"Projection":{"ProjectionType":"ALL"}}]}`)

	for _, item := range typedItems {
		s.ok(t, "PutItem", `{"TableName":"`+typesTable+`","Item":`+item+`}`)
	}
}

// savedItems are the parts of a snapshot that hold its items.
type (
	savedItems struct {
		States []savedState `json:"states"`
	}
	savedState struct {
		Tables []savedTable `json:"tables"`
	}
	savedTable struct {
		Items []jsontext.Value `json:"items"`
	}
)

// snapshotItems is the json of every item of a snapshot, table by table.
func snapshotItems(t *testing.T, file []byte) [][]jsontext.Value {
	t.Helper()

	var saved savedItems

	require.NoError(t, json.Unmarshal(file, &saved))

	var items [][]jsontext.Value

	for _, state := range saved.States {
		for _, table := range state.Tables {
			items = append(items, table.Items)
		}
	}

	return items
}

// scanTyped is the items of typesTable as Scan and a query of the index on g
// answer them.
func scanTyped(t *testing.T, s *stack) string {
	t.Helper()

	scanned := s.call(t, usEast1, "Scan", `{"TableName":"`+typesTable+`"}`)
	require.Equal(t, 200, scanned.status, scanned.body)

	queried := s.call(t, usEast1, "Query", `{"TableName":"`+typesTable+`","IndexName":"by-g",`+
		`"KeyConditionExpression":"g = :g","ExpressionAttributeValues":{":g":{"S":"g2"}}}`)
	require.Equal(t, 200, queried.status, queried.body)

	return scanned.body + "\n" + queried.body
}

// Test_snapshot_loadsMainTypes restores a snapshot main wrote and checks it
// answers as the items put directly do, and saves again as the same file.
func Test_snapshot_loadsMainTypes(t *testing.T) {
	saved, err := os.ReadFile(typesFile)
	require.NoError(t, err)

	restored := newStack(t)
	require.NoError(t, restored.provider.Restore(bytes.NewReader(saved)))

	put := newStack(t)
	putTypedItems(t, put)

	want, got := strings.Split(scanTyped(t, put), "\n"), strings.Split(scanTyped(t, restored), "\n")
	for i := range want {
		require.JSONEq(t, want[i], got[i])
	}

	require.Equal(t, string(saved), string(snapshotOf(t, restored.provider)))
}

// Test_snapshot_writesMainItems puts the items main snapshotted, packed in
// map order, and checks the snapshot writes each item as main wrote it, so
// main restores it as it restores its own; and that the snapshot restores
// here.
func Test_snapshot_writesMainItems(t *testing.T) {
	main, err := os.ReadFile(typesFile)
	require.NoError(t, err)

	s := newStack(t)
	putTypedItems(t, s)

	file := snapshotOf(t, s.provider)
	require.Equal(t, snapshotItems(t, main), snapshotItems(t, file))

	again := newStack(t)
	require.NoError(t, again.provider.Restore(bytes.NewReader(file)))
	require.Equal(t, string(file), string(snapshotOf(t, again.provider)))
}
