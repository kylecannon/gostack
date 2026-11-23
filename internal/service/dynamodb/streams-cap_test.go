package dynamodb_test

import (
	"fmt"
	"runtime"
	"runtime/metrics"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/service/dynamodb"
	"github.com/kylecannon/gostack/internal/service/dynamodb/stream"
	"github.com/stretchr/testify/require"
)

// liveHeap is the bytes of heap objects still reachable: two collections
// first, so what the last one left unswept is swept.
func liveHeap() int64 {
	runtime.GC() //nolint:revive // the measurement is of the heap a collection leaves
	runtime.GC() //nolint:revive // and of what it swept

	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(sample)

	return int64(sample[0].Value.Uint64())
}

// settledHeap is liveHeap once the heap has stopped falling. Other tests of
// the process leave heap that is freed late, after a finalizer, a pool's
// second collection or a goroutine that is winding down has let go of it, so
// one reading can include heap that a later reading does not, and a baseline
// taken before it is freed makes what a test gained read low, or negative.
// Collections repeat, up to settleRounds, until one reads no lower than the
// one before.
func settledHeap() int64 {
	const (
		settleRounds = 50
		settlePause  = 5 * time.Millisecond
	)

	last := liveHeap()

	for range settleRounds {
		time.Sleep(settlePause)

		now := liveHeap()
		if now >= last {
			return now
		}

		last = now
	}

	return last
}

// Test_stream_heapStaysUnderCap: a table written without pause holds its
// stream's records to the shard's byte bound in real heap, not only in
// DynamoDB's size. The writes cycle over a fixed set of keys with an
// OLD_IMAGE stream, so the table holds as much after them as before and what
// the heap gains is the records: each one's old image, a packed item of many
// small attributes. The heap gained stays at or under the bound at every
// checkpoint, and at least half of it, so the bound is what stops it: the
// records are small, so each checkpoint takes enough writes that a bound's
// worth of them is well within the first. Charge errs high by up to half
// (charge_test.go), so the records fill the bound to between two thirds of it
// and all of it, and the half leaves room for the heap's own noise. The heap
// is read once it has settled (settledHeap), so the heap other tests leave,
// and free late, is not counted as what these writes gained or lost.
func Test_stream_heapStaysUnderCap(t *testing.T) {
	const (
		bound       = 4 << 20
		keys        = 16
		attributes  = 40
		checkpoints = 4
		writes      = 2000
	)

	s := newStack(t)
	s.ok(t, "CreateTable", fmt.Sprintf(streamTable, "OLD_IMAGE"))
	shard := dynamodb.Shard(s.provider, usEast1, "streamed")
	shard.SetLimits(stream.Retention, bound)
	t.Cleanup(shard.Stop)

	put := func(n int) {
		attrs := make([]string, attributes)
		for j := range attrs {
			attrs[j] = fmt.Sprintf(`"a%d":{"N":"%d"}`, j, n*attributes+j)
		}

		s.ok(t, "PutItem", fmt.Sprintf(`{"TableName":"streamed","Item":{"id":{"S":"k%d"},%s}}`,
			n%keys, strings.Join(attrs, ",")))
	}

	for n := range keys {
		put(n)
	}

	before := settledHeap()
	n := keys

	for checkpoint := range checkpoints {
		for range writes {
			put(n)
			n++
		}

		gained := settledHeap() - before
		t.Logf("after %d writes the heap gained %d bytes of a %d byte bound", (checkpoint+1)*writes, gained, bound)
		require.LessOrEqual(t, gained, int64(bound), "the records' heap stays within the bound")
		require.GreaterOrEqual(t, gained, int64(bound/2), "the bound, not the writes, stops the records")
	}
}
