package dynamodb_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// benchUpdatePayload is UpdateItem end to end on an item that holds a string
// of the payload's size, which the update leaves as it is: it sets a number,
// alternating between two values so that each write changes the item. A
// stored item that is decoded to update and encoded again pays for its size
// here, where BenchmarkUpdateApply, on an item already decoded, does not. The
// rows sit under BenchmarkUpdateItem with a payload key first, so the rows
// before them keep their names.
//
//	BenchmarkUpdateItem/payload=390KiB/concurrency=1
func benchUpdatePayload(b *testing.B) {
	b.Helper()

	for _, payload := range []struct {
		name string
		size int
	}{{"64KiB", 64 << 10}, {"390KiB", 390 << 10}} {
		b.Run("payload="+payload.name+"/concurrency=1", func(b *testing.B) {
			s := newStack(b)
			s.ok(b, opCreate, hashTable(benchTable))
			s.ok(b, opPut, fmt.Sprintf(`{"TableName":%q,"Item":{"id":{"S":"k"},"n":{"N":"0"},"d":{"S":%q}}}`,
				benchTable, strings.Repeat("x", payload.size)))

			bodies := [2]string{}
			for i := range bodies {
				bodies[i] = fmt.Sprintf(`{"TableName":%q,"Key":{"id":{"S":"k"}},"UpdateExpression":"SET n = :v",`+
					`"ExpressionAttributeValues":{":v":{"N":"%d"}}}`, benchTable, i+1)
			}

			b.SetBytes(int64(payload.size))
			b.ReportAllocs()

			i := 0
			for b.Loop() {
				got := s.call(b, usEast1, opUpdateItem, bodies[i%2])
				require.Equal(b, http.StatusOK, got.status, got.body)

				i++
			}
		})
	}
}
