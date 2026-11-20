package dynamodb_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/stretchr/testify/require"
)

// BenchmarkRemoveExpired is the expired-item sweep over one table of a given
// size, with every item expired or none. One op is one sweep; with every item
// expired the table is filled again, untimed, before each sweep. Names carry
// the axes, for example
//
//	BenchmarkRemoveExpired/items=10000/expired=all
//
// 1,000,000 items, 0.5 s to 1.4 s a sweep, run only under GOSTACK_BENCH_LARGE=1.
func BenchmarkRemoveExpired(b *testing.B) {
	sizes := []int{1000, 10000}
	if os.Getenv(largeEnv) == "1" {
		sizes = append(sizes, 1000000)
	}

	for _, items := range sizes {
		for _, all := range []bool{false, true} {
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
