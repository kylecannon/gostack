package dynamodb_test

import (
	"bytes"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/persist"
	"github.com/stretchr/testify/require"
)

// gsiOn is an UpdateTable body that creates index gsi on attribute gk of
// type typ in table name.
func gsiOn(name, gsi, typ string) string {
	return `{"TableName":"` + name + `","AttributeDefinitions":[{"AttributeName":"gk","AttributeType":"` + typ + `"}],` +
		`"GlobalSecondaryIndexUpdates":[{"Create":{"IndexName":"` + gsi + `","KeySchema":` +
		`[{"AttributeName":"gk","KeyType":"HASH"}],"Projection":{"ProjectionType":"ALL"},` +
		`"ProvisionedThroughput":{"ReadCapacityUnits":5,"WriteCapacityUnits":5}}}]}`
}

// Test_restore_indexLeavesOutMismatchedItems adds an index over stored items
// whose index key attribute is of another type, or an empty string or binary.
// The index leaves them out, and a restart brings back the table, every item
// and the same index.
func Test_restore_indexLeavesOutMismatchedItems(t *testing.T) {
	for _, format := range []persist.Format{persist.FormatJSON, persist.FormatBinary} {
		for name, gk := range map[string]string{
			"number":       n5,
			"empty string": emptyS,
			"empty binary": `{"B":""}`,
		} {
			t.Run(string(format)+"/"+name, func(t *testing.T) {
				dir := t.TempDir()
				before, eng := formatStack(t, dir, format)
				before.ok(t, opCreate, hashTable("live"))
				before.ok(t, opCreate, hashTable("other"))
				before.ok(t, opPut, `{"TableName":"other","Item":{"id":{"S":"o"}}}`)
				before.ok(t, opPut, `{"TableName":"live","Item":{"id":{"S":"a"},"gk":`+gk+`}}`)
				before.ok(t, opPut, `{"TableName":"live","Item":{"id":{"S":"b"},"gk":{"S":"x"}}}`)
				before.ok(t, opUpdate, gsiOn("live", "gsi1", "S"))

				index := `{"TableName":"live","IndexName":"gsi1"}`
				require.JSONEq(t, `{"Count":1,"ScannedCount":1,"Items":[{"id":{"S":"b"},"gk":{"S":"x"}}]}`,
					before.call(t, usEast1, opScan, index).body)

				require.NoError(t, eng.Flush(t.Context()))

				after, _ := formatStack(t, dir, format)
				require.JSONEq(t, `{"TableNames":["live","other"]}`, after.call(t, usEast1, opList, `{}`).body)
				require.JSONEq(t, `{"Item":{"id":{"S":"a"},"gk":`+gk+`}}`,
					after.call(t, usEast1, opGet, `{"TableName":"live","Key":{"id":{"S":"a"}}}`).body)

				for _, body := range []string{index, `{"TableName":"live"}`, `{"TableName":"other"}`} {
					sameAnswer(t, before, after, opScan, body)
				}

				sameAnswer(t, before, after, opDescribe, `{"TableName":"live"}`)
			})
		}
	}
}

// restoreSeeds are operation sequences for FuzzRestoreAnswers: an index
// added over an item whose index key is a number, an empty string, and
// binary, on both tables, and indexes dropped, tables deleted and created
// again in between.
var restoreSeeds = [][]byte{
	{0, 0, 0, 0, 3, 0, 0, 4, 4, 0, 0, 0},
	{0, 0, 0, 0, 3, 0, 1, 3, 3, 0, 2, 1, 4, 0, 0, 0},
	{0, 1, 0, 0, 2, 1, 1, 6, 4, 1, 1, 1, 4, 1, 0, 2, 5, 1, 0, 0, 7, 1, 1, 0, 6, 1, 1, 0},
	{0, 0, 0, 0, 3, 0, 0, 7, 4, 0, 0, 2, 1, 0, 0, 0, 0, 0, 0, 0, 3, 0, 1, 4, 4, 0, 0, 1},
}

// FuzzRestoreAnswers runs a sequence of table and item operations decoded
// from the fuzz bytes, index creation and deletion and items whose index key
// is of another type among them, then snapshots the state and restores it:
// every table must come back, and DescribeTable, Scan and Query of each table
// and index must answer as they did before. The persistence engine writes the
// one snapshot to the file of each format alike, so the round trip here is in
// memory; Test_restore_answersInEachFormat runs the seeds through the engine.
func FuzzRestoreAnswers(f *testing.F) {
	for _, seed := range restoreSeeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > maxFuzzOps*opWidth {
			return
		}

		before := newStack(t)
		run := runOps(t, before, data)

		after := newStack(t)
		require.NoError(t, after.provider.Restore(bytes.NewReader(snapshotOf(t, before.provider))))

		for _, q := range run.questions() {
			sameAnswer(t, before, after, q.op, q.body)
		}
	})
}

// Test_restore_answersInEachFormat is FuzzRestoreAnswers' check for each seed
// through the persistence engine, in each format.
func Test_restore_answersInEachFormat(t *testing.T) {
	for _, format := range []persist.Format{persist.FormatJSON, persist.FormatBinary} {
		for i, seed := range restoreSeeds {
			t.Run(string(format)+"/"+strconv.Itoa(i), func(t *testing.T) {
				dir := t.TempDir()
				before, eng := formatStack(t, dir, format)
				run := runOps(t, before, seed)

				require.NoError(t, eng.Flush(t.Context()))

				after, _ := formatStack(t, dir, format)
				for _, q := range run.questions() {
					sameAnswer(t, before, after, q.op, q.body)
				}
			})
		}
	}
}

// runOps applies the operations data decodes to.
func runOps(tb testing.TB, s *stack, data []byte) *fuzzRun {
	tb.Helper()

	run := &fuzzRun{s: s}

	for op := range slices.Chunk(data, opWidth) {
		if len(op) == opWidth {
			run.apply(tb, op)
		}
	}

	return run
}

// sameAnswer checks that before and after answer operation with body alike:
// the same status and the same JSON, whatever the order of an item's
// attributes.
func sameAnswer(tb testing.TB, before, after *stack, operation, body string) {
	tb.Helper()

	want, got := before.call(tb, usEast1, operation, body), after.call(tb, usEast1, operation, body)
	require.Equal(tb, want.status, got.status, "%s %s: %s", operation, body, got.body)
	require.JSONEq(tb, want.body, got.body, "%s %s", operation, body)
}

// emptyS is an empty string value; fuzzTab0 is the fuzz run's table without a
// range key, tab1 the one with.
const (
	emptyS   = `{"S":""}`
	fuzzTab0 = "tab0"
)

// The shape of FuzzRestoreAnswers' input: opWidth bytes an operation, at most
// maxFuzzOps of them.
const (
	opWidth    = 4
	maxFuzzOps = 64
)

// The fuzz run's vocabulary: two tables, one with a range key; the index key
// values an item can carry, absent among them; the types an index can take.
var (
	fuzzTables = []string{fuzzTab0, tab1}
	fuzzValues = []string{``, sX, `{"S":"y"}`, emptyS, n5, `{"N":"-1.5"}`, `{"B":"AQ=="}`, `{"B":""}`}
	fuzzTypes  = []string{"S", "N", "B"}
)

// fuzzRun applies decoded operations and remembers what to ask afterwards.
type fuzzRun struct {
	s *stack
}

type question struct{ op, body string }

// apply runs one operation: op[0] picks it, op[1] the table, op[2] and op[3]
// its arguments. Any answer is fine; what counts is the state left behind.
func (r *fuzzRun) apply(tb testing.TB, op []byte) {
	tb.Helper()

	name := fuzzTables[int(op[1])%len(fuzzTables)]
	id := strconv.Itoa(int(op[2]) % 4)
	gk := fuzzValues[int(op[3])%len(fuzzValues)]
	gsi := "gsi" + strconv.Itoa(int(op[2])%2)

	var operation, body string

	switch op[0] % 8 {
	case 0:
		operation, body = opCreate, fuzzTable(name)
	case 1:
		operation, body = opDelete, `{"TableName":"`+name+`"}`
	case 2, 3:
		operation, body = opPut, `{"TableName":"`+name+`","Item":`+fuzzItem(name, id, gk, int(op[0]%3))+`}`
	case 4:
		operation = opUpdate
		body = strings.Replace(gsiOn(name, gsi, fuzzTypes[int(op[3])%len(fuzzTypes)]), `"gk"`, `"`+gsiAttr(gsi)+`"`, 2)
	case 5:
		operation = opUpdate
		body = `{"TableName":"` + name + `","GlobalSecondaryIndexUpdates":[{"Delete":{"IndexName":"` + gsi + `"}}]}`
	case 6:
		operation, body = opDeleteI, `{"TableName":"`+name+`","Key":`+fuzzKey(name, id)+`}`
	default:
		operation, body = opUpdateItem, `{"TableName":"`+name+`","Key":`+fuzzKey(name, id)+
			`,"UpdateExpression":"SET h = :v","ExpressionAttributeValues":{":v":{"N":"`+id+`"}}}`
	}

	got := r.s.call(tb, usEast1, operation, body)
	require.NotEqual(tb, http.StatusInternalServerError, got.status, got.body)
}

// gsiAttr is the attribute index gsi is keyed on: gsi0 on gk, gsi1 on gr.
func gsiAttr(gsi string) string {
	if gsi == "gsi0" {
		return "gk"
	}

	return "gr"
}

// fuzzTable is the CreateTable body of a fuzz table: tab0 keyed by id, tab1 by id
// and a number range key r.
func fuzzTable(name string) string {
	if name == fuzzTab0 {
		return hashTable(name)
	}

	return `{"TableName":"tab1","KeySchema":[{"AttributeName":"id","KeyType":"HASH"},` +
		`{"AttributeName":"r","KeyType":"RANGE"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"},` +
		`{"AttributeName":"r","AttributeType":"N"}],"BillingMode":"PAY_PER_REQUEST"}`
}

func fuzzKey(name, id string) string {
	if name == fuzzTab0 {
		return `{"id":{"S":"` + id + `"}}`
	}

	return `{"id":{"S":"` + id + `"},"r":{"N":"1"}}`
}

// fuzzItem is an item keyed id carrying gk, when not empty, as the attribute
// which picks: gk, gr or both.
func fuzzItem(name, id, gk string, which int) string {
	key := fuzzKey(name, id)
	if gk == "" {
		return key
	}

	attrs := []string{`"gk":` + gk, `"gr":` + gk, `"gk":` + gk + `,"gr":` + gk}[which]

	return key[:len(key)-1] + `,` + attrs + `}`
}

// questions are the reads compared before and after the restart.
func (*fuzzRun) questions() []question {
	qs := make([]question, 0, 64)
	qs = append(qs, question{opList, `{}`})

	for _, name := range fuzzTables {
		table := `{"TableName":"` + name + `"`
		qs = append(qs, question{opDescribe, table + `}`}, question{opScan, table + `}`})

		for _, gsi := range []string{"gsi0", "gsi1"} {
			index := table + `,"IndexName":"` + gsi + `"`
			qs = append(qs, question{opScan, index + `}`})

			for _, v := range fuzzValues[1:] {
				qs = append(qs, question{opQuery, index + `,"KeyConditionExpression":"#k = :v",` +
					`"ExpressionAttributeNames":{"#k":"` + gsiAttr(gsi) + `"},"ExpressionAttributeValues":{":v":` + v + `}}`})
			}
		}

		for id := range 4 {
			qs = append(qs, question{opQuery, table + `,"KeyConditionExpression":"id = :v",` +
				`"ExpressionAttributeValues":{":v":{"S":"` + strconv.Itoa(id) + `"}}}`})
		}
	}

	return qs
}
