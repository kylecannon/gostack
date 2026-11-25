package dynamodb_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/stretchr/testify/require"
)

// maxExpiredAllItems is the most items a sweep that removes all of them takes
// (tooSlowToGate), 1.3 us each on a 4 CPU runner: 5,000 would be about 6.5 ms,
// too near the limit for a run that is slower. The large counts under
// largeEnv are not cut.
const maxExpiredAllItems = 3_000

// BenchmarkRemoveExpired is the expired-item sweep over one table of a given
// size, with every item expired or none. One op is one sweep; with every item
// expired the table is filled again, untimed, before each sweep. Names carry
// the axes, for example
//
//	BenchmarkRemoveExpired/items=3000/expired=all
//
// 1,000,000 items, 0.5 s to 1.4 s a sweep, run only under GOSTACK_BENCH_LARGE=1.
//
// A sweep that removes every item is cut per count, at maxExpiredAllItems:
// 10,000 items took 12.4 to 13.1 ms on the gate's runners (ten runs, bench run
// 45505878281 of PR 84, main a0e13bb3), over the 8 ms a cell is held to, and
// 1,000 took 1.2 to 1.3 ms, 1.3 us an item. A sweep that removes none, which
// only reads the items, keeps 10,000 (2.2 to 2.5 ms). The sweep of all of them
// keeps a second count at 3,000, which the runners did not time: 1.3 us an item
// is about 3.9 ms, and a Mac of 10 CPUs took 3.7 ms at the slowest of five runs
// (local).
func BenchmarkRemoveExpired(b *testing.B) {
	large := os.Getenv(largeEnv) == "1"

	sizes := []int{1000, 3000, 10000}
	if large {
		sizes = append(sizes, 1000000)
	}

	for _, items := range sizes {
		for _, all := range []bool{false, true} {
			if all && !large && tooSlowToGate(items, maxExpiredAllItems) {
				continue
			}

			name := "none"
			if all {
				name = "all"
			}

			b.Run(fmt.Sprintf("items=%d/expired=%s", items, name), func(b *testing.B) {
				s := newStack(b)
				s.ok(b, opCreate, hashTable(benchTable))
				s.ok(b, "UpdateTimeToLive", `{"TableName":"`+benchTable+
					`","TimeToLiveSpecification":{"Enabled":true,"AttributeName":"ttl"}}`)

				ttl := "99999999999"
				if all {
					ttl = "1"
				}

				fillTTL(b, s, items, ttl)
				b.ResetTimer()

				for b.Loop() {
					removed := dynamodb.RemoveExpired(s.provider)
					if all {
						b.StopTimer()
						require.Equal(b, items, removed)
						fillTTL(b, s, items, ttl)
						b.StartTimer()
					}
				}
			})
		}
	}
}

// fillTTL writes n items to the bench table, each with the given ttl, 25
// to a BatchWriteItem.
func fillTTL(b *testing.B, s *stack, n int, ttl string) {
	b.Helper()

	const batch = 25

	var body strings.Builder

	for start := 0; start < n; start += batch {
		body.Reset()
		body.WriteString(`{"RequestItems":{"` + benchTable + `":[`)

		for i := start; i < min(start+batch, n); i++ {
			if i > start {
				body.WriteByte(',')
			}

			fmt.Fprintf(&body, `{"PutRequest":{"Item":{"id":{"S":"k-%d"},"ttl":{"N":"%s"}}}}`, i, ttl)
		}

		body.WriteString(`]}}`)
		s.ok(b, "BatchWriteItem", body.String())
	}
}
