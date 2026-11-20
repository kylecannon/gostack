package persist

import (
	"context"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slowName is the service of a slow Snapshot that sorts before dynamodb.
const slowName = "athena"

// countingSnap is a stateSnap that counts its Snapshots.
type countingSnap struct {
	*stateSnap

	snapshots atomic.Int32
}

func (c *countingSnap) Snapshot(w io.Writer) error {
	c.snapshots.Add(1)

	return c.stateSnap.Snapshot(w)
}

func (c *countingSnap) count() int { return int(c.snapshots.Load()) }

// Test_Engine_ShutdownBehindAQueuedService: the timed save took sqs, v2 and
// acknowledged, together with dynamodb, and sqs waits its turn behind
// dynamodb's 2 s Snapshot, which cannot be cut short, when the shutdown
// arrives. Stop, under a 300 ms save deadline, moves sqs to its own flush,
// whose save the timed one had not begun, and saves it beside the timed one.
// The timed save then skips sqs: it is snapshotted once.
func Test_Engine_ShutdownBehindAQueuedService(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		e := New(quietConfig(dir, 10*time.Millisecond))
		big := verySlow{stateSnap: &stateSnap{state: "dynamodb tables"}, d: 2 * time.Second, started: make(chan struct{})}
		sqs := &countingSnap{stateSnap: &stateSnap{state: sqsV1}}
		e.Register(ddb, big)
		e.Register(sqsName, sqs)
		sqsFile := filepath.Join(dir, sqsName, FormatJSON.fileName())

		e.MarkDirty(sqsName)
		require.NoError(t, e.Flush(t.Context()))

		sqs.set(sqsAcked)
		e.MarkDirty(sqsName)
		e.MarkDirty(ddb)
		<-big.started // the timed save took both, and is in dynamodb's Snapshot first
		assert.Equal(t, sqsV1, readState(t, sqsFile), "sqs waits its turn")

		saveCtx, cancelSave := context.WithTimeout(context.WithoutCancel(t.Context()), 300*time.Millisecond)
		defer cancelSave()

		before := sqs.count()
		_ = e.Stop(saveCtx)
		assert.Equal(t, sqsAcked, readState(t, sqsFile), "Stop saved the queued sqs")

		time.Sleep(3 * time.Second) // the timed save ends
		synctest.Wait()
		assert.Equal(t, before+1, sqs.count(), "sqs is snapshotted once, by Stop's flush")
	})
}

// Test_Engine_FlushBesideMovesAQueuedPairTogether: the timed save took a slow
// service and the dynamodb pair, and is in the slow one's Snapshot. A Flush
// beside it, under a 300 ms deadline, moves the pair, whose saves the timed
// one had not begun, to itself, and saves it before the slow Snapshot ends:
// the leader is told true, the table and the streams are snapshotted
// in turn and the leader is told false, once. The timed save skips both.
func Test_Engine_FlushBesideMovesAQueuedPairTogether(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		e := New(quietConfig(dir, 10*time.Millisecond))
		log := &orderLog{}
		big := verySlow{stateSnap: &stateSnap{state: "slow state"}, d: 2 * time.Second, started: make(chan struct{})}
		table := &stateSnap{state: "table v2"}
		streams := &stateSnap{state: "streams v2"}
		e.Register(slowName, big) // sorts before dynamodb, so the timed save begins with it
		e.Register(ddb, recLeader{stateSnap: table, log: log})
		e.Register(ddbStream, recFollower{stateSnap: streams, log: log})

		e.MarkDirty(slowName)
		e.MarkDirty(ddb)
		<-big.started // the timed save took all three, and is in big's Snapshot

		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()

		require.ErrorIs(t, e.Flush(ctx), context.DeadlineExceeded, "the slow Snapshot outlasts the Flush")

		file := func(name string) string { return readState(t, filepath.Join(dir, name, FormatJSON.fileName())) }
		assert.Equal(t, "table v2", file(ddb), "saved beside the slow Snapshot, before it ends")
		assert.Equal(t, "streams v2", file(ddbStream))

		time.Sleep(3 * time.Second) // the timed save ends
		synctest.Wait()

		log.mu.Lock()
		defer log.mu.Unlock()

		assert.Equal(t, []string{"followed true", ddb, ddbStream, "followed false"}, log.seen,
			"the pair moved together, and is saved once")
	})
}
