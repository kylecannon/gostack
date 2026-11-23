package dynamodb_test

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
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

// settledHeap is liveHeap once the heap has stopped falling. Heap can be
// freed late, after a finalizer, a pool's second collection or a goroutine
// that is winding down has let go of it, so one reading can include heap that
// a later reading does not, and a baseline taken before it is freed makes
// what a test gained read low, or negative. Collections repeat, up to
// settleRounds, until one reads no lower than the one before. It cannot wait
// out a holder that is still alive when it reads: heap held a second longer
// reads as settled, which is why Test_stream_heapStaysUnderCap runs alone.
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

// aloneEnv marks the process runAlone starts.
const aloneEnv = "GOSTACK_TEST_ALONE"

// runAlone reports whether the calling test is running alone in a process of
// its own, and if not, runs it so in the test binary and fails unless it
// passed. A test that measures the process's heap calls it first, and
// returns at once when it reports false.
func runAlone(t *testing.T) bool {
	t.Helper()

	if os.Getenv(aloneEnv) == t.Name() {
		return true
	}

	cmd := exec.CommandContext(t.Context(), os.Args[0],
		"-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1", "-test.v", "-test.timeout=10m")
	cmd.Env = append(os.Environ(), aloneEnv+"="+t.Name())

	out, err := cmd.CombinedOutput()
	t.Logf("alone:\n%s", out)
	require.NoError(t, err, "the test passes alone")
	require.Contains(t, string(out), "--- PASS: "+t.Name(), "the test ran alone")

	return false
}

// Test_stream_heapStaysUnderCap: a table written without pause holds its
// stream's records to the shard's byte bound in real heap, not only in
// DynamoDB's size. The writes cycle over a fixed set of keys with an
// OLD_IMAGE stream, so the table holds as much after them as before and what
// the heap gains is the records: each one's old image, a packed item of many
// small attributes. The heap gained stays at or under the bound at every
// checkpoint, and at least half of it, so the bound is what stops it. The
// records are small, so the shard fills about 2,400 writes in and a
// checkpoint takes 4,000: the first is already past the fill, and the last
// three, which only trim, gain within a few percent of one another, so a
// record the trim leaves held, one per write, would show as growth. Charge
// errs high by up to half (charge_test.go), so the records fill the bound to
// between two thirds of it and all of it, and the half leaves room for the
// heap's own noise. The test runs alone in a process of its own (runAlone):
// heap another test of the package leaves held, and frees only after the
// baseline is read, read as a loss of up to 4.8 MB, and settledHeap cannot
// wait out a holder still alive when it reads.
func Test_stream_heapStaysUnderCap(t *testing.T) {
	if !runAlone(t) {
		return
	}

	const (
		bound       = 4 << 20
		keys        = 16
		attributes  = 40
		checkpoints = 4
		writes      = 4000
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

	var steady int64

	for checkpoint := range checkpoints {
		for range writes {
			put(n)
			n++
		}

		gained := settledHeap() - before
		t.Logf("after %d writes the heap gained %d bytes of a %d byte bound", (checkpoint+1)*writes, gained, bound)
		require.LessOrEqual(t, gained, int64(bound), "the records' heap stays within the bound")
		require.GreaterOrEqual(t, gained, int64(bound/2), "the bound, not the writes, stops the records")

		if checkpoint == 1 {
			steady = gained
		}

		if checkpoint > 1 {
			require.InDelta(t, steady, gained, bound/20, "a full shard's heap does not grow with the writes")
		}
	}
}
