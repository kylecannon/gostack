package dynamodb_test

import (
	"bytes"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Test_snapshot_oneCut runs transactions that add one to a counter in each
// of two tables beside snapshots. Every snapshot must restore with the two
// counters equal: a snapshot is one moment of every table, never a
// transaction applied in one table and not yet in the other. The first table
// holds filler items, so encoding it takes long enough for transactions to
// land before the second table would be copied.
func Test_snapshot_oneCut(t *testing.T) {
	s := newStack(t)
	s.ok(t, opCreate, hashTable("cut-a"))
	s.ok(t, opCreate, hashTable("cut-b"))

	filler := strings.Repeat("f", 200)

	for batch := range 400 {
		puts := make([]string, 25)
		for i := range puts {
			puts[i] = `{"PutRequest":{"Item":{"id":{"S":"f` + strconv.Itoa(batch*25+i) + `"},"pad":{"S":"` + filler + `"}}}}`
		}

		s.ok(t, opBatchWrite, `{"RequestItems":{"cut-a":[`+strings.Join(puts, ",")+`]}}`)
	}

	const add = `{"Update":{"TableName":"%s","Key":{"id":{"S":"ctr"}},"UpdateExpression":"ADD c :one",` +
		`"ExpressionAttributeValues":{":one":{"N":"1"}}}}`

	txn := `{"TransactItems":[` + strings.ReplaceAll(add, "%s", "cut-a") + `,` +
		strings.ReplaceAll(add, "%s", "cut-b") + `]}`

	stop := make(chan struct{})

	var wg sync.WaitGroup

	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}

				s.call(t, usEast1, opTransactWrite, txn)
			}
		})
	}

	defer func() {
		close(stop)
		wg.Wait()
	}()

	counter := func(r *stack, table string) string {
		return r.call(t, usEast1, opGet, `{"TableName":"`+table+`","Key":{"id":{"S":"ctr"}}}`).body
	}

	deadline := time.Now().Add(time.Second)
	for i := 0; i < 5 || time.Now().Before(deadline); i++ {
		restored := newStack(t)
		require.NoError(t, restored.provider.Restore(bytes.NewReader(snapshotOf(t, s.provider))))

		require.JSONEq(t, counter(restored, "cut-a"), counter(restored, "cut-b"),
			"snapshot %d holds one table's counter without the other's", i)
	}
}
