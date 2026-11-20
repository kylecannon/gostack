package dynamodb_test

import (
	"bytes"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/stream"
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
	fillCutTable(t, s, "cut-a")
	runCutTransactions(t, s, "cut-a", "cut-b")

	deadline := time.Now().Add(time.Second)
	for i := 0; i < 5 || time.Now().Before(deadline); i++ {
		restored := newStack(t)
		require.NoError(t, restored.provider.Restore(bytes.NewReader(snapshotOf(t, s.provider))))

		require.JSONEq(t, cutCounter(t, restored, "cut-a"), cutCounter(t, restored, "cut-b"),
			"snapshot %d holds one table's counter without the other's", i)
	}
}

// Test_snapshot_streamsAtTheCut runs the transactions of
// Test_snapshot_oneCut over two tables with streams, takes the streams
// snapshot right after each table snapshot, as a flush does, and restores
// both. Each restored table's counter must equal the number of MODIFY
// records its shard holds for the counter, and the two shards must agree: the
// streams file holds the records of the writes the table cut holds and none
// after it, however long the table snapshot took to encode. The second case
// restores the streams file of a later cut over the tables of an earlier one,
// which restore trims to the tables' positions.
func Test_snapshot_streamsAtTheCut(t *testing.T) {
	s := newStack(t)

	s.ok(t, opCreate, hashTable("cut-a"))
	s.ok(t, opCreate, hashTable("cut-b"))
	fillCutTable(t, s, "cut-a") // before the stream, which then holds only the counter's records

	for _, name := range []string{"cut-a", "cut-b"} {
		s.ok(t, "UpdateTable", `{"TableName":"`+name+`",`+
			`"StreamSpecification":{"StreamEnabled":true,"StreamViewType":"KEYS_ONLY"}}`)
		s.ok(t, opPut, `{"TableName":"`+name+`","Item":{"id":{"S":"ctr"},"c":{"N":"0"}}}`)
	}

	runCutTransactions(t, s, "cut-a", "cut-b")

	// check restores tables and streams and checks the counters against the
	// records. It returns the restored provider's streams snapshot after a
	// table save.
	check := func(t *testing.T, i int, tables, streams []byte) []byte {
		t.Helper()

		restored := newStack(t)
		require.NoError(t, restored.provider.Restore(bytes.NewReader(tables)))
		require.NoError(t, restored.provider.RestoreStreams(bytes.NewReader(streams)))

		var updates [2]int

		for j, name := range []string{"cut-a", "cut-b"} {
			counter := restored.ok(t, opGet, `{"TableName":"`+name+`","Key":{"id":{"S":"ctr"}}}`)
			c, err := strconv.Atoi(path(t, counter, "Item", "c", "N").(string)) //nolint:forcetypeassert // a number
			require.NoError(t, err)

			for _, r := range dynamodb.Shard(restored.provider, usEast1, name).Records() {
				if r.Event == stream.Modify && isCounter(r) {
					updates[j]++
				}
			}

			require.Equal(t, c, updates[j], "snapshot %d: table %s's counter against its stream's update records", i, name)
		}

		require.Equal(t, updates[0], updates[1], "snapshot %d: the two shards disagree", i)

		savedOf(t, restored.provider)

		var again bytes.Buffer
		require.NoError(t, restored.provider.SnapshotStreams(&again))

		return again.Bytes()
	}

	streamsOf := func(t *testing.T) []byte {
		t.Helper()

		var buf bytes.Buffer
		require.NoError(t, s.provider.SnapshotStreams(&buf))

		return buf.Bytes()
	}

	t.Run("one flush", func(t *testing.T) {
		deadline := time.Now().Add(time.Second)
		for i := 0; i < 3 || time.Now().Before(deadline); i++ {
			tables, streams := savedOf(t, s.provider), streamsOf(t)
			require.Equal(t, string(streams), string(check(t, i, tables, streams)),
				"a restored provider's streams snapshot, after its first table save, is the one it loaded")
		}
	})

	t.Run("streams of a later cut", func(t *testing.T) {
		for i := range 3 {
			tables := savedOf(t, s.provider)
			s.ok(t, opTransactWrite, cutTxn("cut-a", "cut-b")) // the later cut holds at least one more
			savedOf(t, s.provider)
			check(t, i, tables, streamsOf(t))
		}
	})
}

// fillCutTable puts 10,000 filler items of 200 bytes in table, so encoding it
// takes long enough for transactions to land meanwhile.
func fillCutTable(t *testing.T, s *stack, table string) {
	t.Helper()

	filler := strings.Repeat("f", 200)

	for batch := range 400 {
		puts := make([]string, 25)
		for i := range puts {
			puts[i] = `{"PutRequest":{"Item":{"id":{"S":"f` + strconv.Itoa(batch*25+i) + `"},"pad":{"S":"` + filler + `"}}}}`
		}

		s.ok(t, opBatchWrite, `{"RequestItems":{"`+table+`":[`+strings.Join(puts, ",")+`]}}`)
	}
}

// maxCutTransactions bounds the transactions runCutTransactions runs, so a
// slow run never has the stream's byte bound trim a counter's records.
const maxCutTransactions = 8_000

// runCutTransactions runs four clients of transactions that add one to the
// counter item ctr of tables a and b together, until the test ends or
// maxCutTransactions have run.
func runCutTransactions(t *testing.T, s *stack, a, b string) {
	t.Helper()

	txn := cutTxn(a, b)
	stop := make(chan struct{})

	var (
		wg  sync.WaitGroup
		ran atomic.Int64
	)

	for range 4 {
		wg.Go(func() {
			for ran.Add(1) <= maxCutTransactions {
				select {
				case <-stop:
					return
				default:
				}

				s.call(t, usEast1, opTransactWrite, txn)
			}
		})
	}

	t.Cleanup(func() {
		close(stop)
		wg.Wait()
	})
}

// cutTxn is a transaction that adds one to the counter item ctr of tables a
// and b.
func cutTxn(a, b string) string {
	const add = `{"Update":{"TableName":"%s","Key":{"id":{"S":"ctr"}},"UpdateExpression":"ADD c :one",` +
		`"ExpressionAttributeValues":{":one":{"N":"1"}}}}`

	return `{"TransactItems":[` + strings.ReplaceAll(add, "%s", a) + `,` + strings.ReplaceAll(add, "%s", b) + `]}`
}

// isCounter reports whether r is a record of the counter item ctr.
func isCounter(r stream.Record) bool {
	id := r.Keys["id"].S

	return id != nil && *id == "ctr"
}

// cutCounter is GetItem's answer for the counter item of table.
func cutCounter(t *testing.T, s *stack, table string) string {
	t.Helper()

	return s.call(t, usEast1, opGet, `{"TableName":"`+table+`","Key":{"id":{"S":"ctr"}}}`).body
}
