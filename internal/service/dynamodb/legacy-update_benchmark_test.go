package dynamodb_test

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// legacyAction is one Action of AttributeUpdates, the value an attribute holds
// for it to act on, and the entry that names attribute i.
type legacyAction struct {
	name, held string
	entry      func(i int) string
}

// BenchmarkUpdateItemLegacy is BenchmarkUpdateItem's matrix for the legacy
// AttributeUpdates: by the number of entries, by the Action, and by
// concurrency. Every Action works on attributes the item holds, a number for
// ADD and a set for DELETE, which takes the same member out each call.
//
//	BenchmarkUpdateItemLegacy/actions=10/action=ADD/concurrency=16
func BenchmarkUpdateItemLegacy(b *testing.B) {
	actions := []legacyAction{
		{"PUT", `{"N":"0"}`, func(i int) string { return fmt.Sprintf(`"a%d":{"Action":"PUT","Value":{"N":"1"}}`, i) }},
		{"ADD", `{"N":"0"}`, func(i int) string { return fmt.Sprintf(`"a%d":{"Action":"ADD","Value":{"N":"1"}}`, i) }},
		{"DELETE", `{"SS":["x","y"]}`, func(i int) string {
			return fmt.Sprintf(`"a%d":{"Action":"DELETE","Value":{"SS":["x"]}}`, i)
		}},
	}

	for _, entries := range updateActions {
		for _, action := range actions {
			for _, workers := range readConcurrency {
				b.Run(fmt.Sprintf("actions=%d/action=%s/concurrency=%d", entries, action.name, workers), func(b *testing.B) {
					s := newStack(b)
					s.ok(b, opCreate, hashTable(benchTable))

					members := make([]string, entries)
					stored := make([]string, entries)

					for i := range members {
						members[i] = action.entry(i)
						stored[i] = `"a` + strconv.Itoa(i) + `":` + action.held
					}

					updates := strings.Join(members, ",")
					bodies := make([]string, workers)

					for worker := range workers {
						s.ok(b, opPut, fmt.Sprintf(`{"TableName":%q,"Item":{"id":{"S":"k-%d"},%s}}`,
							benchTable, worker, strings.Join(stored, ",")))

						bodies[worker] = fmt.Sprintf(`{"TableName":%q,"Key":{"id":{"S":"k-%d"}},"AttributeUpdates":{%s}}`,
							benchTable, worker, updates)
					}

					b.SetBytes(int64(len(updates) * workers))
					b.ReportAllocs()

					for b.Loop() {
						require.NoError(b, s.fanOut(b, opUpdateItem, workers, func(worker int) string { return bodies[worker] }))
					}
				})
			}
		}
	}
}
