package persist

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The states of sqs in the tests of a flush beside a timed one.
const (
	sqsV1    = "sqs v1"
	sqsAcked = "sqs v2 acknowledged before SIGTERM"
)

// verySlow is a service whose Snapshot takes d and cannot be interrupted, and
// which says when one starts.
type verySlow struct {
	*stateSnap

	d       time.Duration
	started chan struct{}
}

func (s verySlow) Snapshot(w io.Writer) error {
	select {
	case s.started <- struct{}{}:
	default:
	}

	time.Sleep(s.d)

	return s.stateSnap.Snapshot(w)
}

// followingSnap is a stateSnap that follows leader.
type followingSnap struct {
	*stateSnap

	leader string
}

func (f followingSnap) Follows() string { return f.leader }

// Test_Engine_ShutdownBesideAnotherServicesLongSnapshot is review SQ-295 A2: a
// timed save is inside dynamodb's 2 s Snapshot, which cannot be cut short,
// when the shutdown arrives, and sqs is acknowledged after that save took its
// services. The shutdown is saver.save's: Stop under a 300 ms save deadline,
// then Close under a 700 ms close deadline, and the kill at 800 ms. The
// shutdown saves sqs beside the timed save, so the acknowledged write is on
// disk when Stop returns.
func Test_Engine_ShutdownBesideAnotherServicesLongSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		e := New(quietConfig(dir, 10*time.Millisecond))
		big := verySlow{stateSnap: &stateSnap{state: "big"}, d: 2 * time.Second, started: make(chan struct{})}
		sqs := &stateSnap{state: sqsV1}
		e.Register(ddb, big)
		e.Register(sqsName, sqs)
		sqsFile := filepath.Join(dir, sqsName, FormatJSON.fileName())

		e.MarkDirty(sqsName)
		require.NoError(t, e.Flush(t.Context()))

		e.MarkDirty(ddb)
		<-big.started // the timed save is inside the dynamodb Snapshot
		time.Sleep(10 * time.Millisecond)

		sqs.set(sqsAcked)
		e.MarkDirty(sqsName)

		saveCtx, cancelSave := context.WithTimeout(context.WithoutCancel(t.Context()), 300*time.Millisecond)
		defer cancelSave()

		closeCtx, cancelClose := context.WithTimeout(context.WithoutCancel(t.Context()), 700*time.Millisecond)
		defer cancelClose()

		err := e.Stop(saveCtx)
		require.ErrorIs(t, err, context.DeadlineExceeded, "dynamodb's save outlasts the save deadline")
		assert.Equal(t, sqsAcked, readState(t, sqsFile),
			"Stop saved sqs beside the timed save")

		_ = e.Close(closeCtx)
		assert.Equal(t, sqsAcked, readState(t, sqsFile), "at the kill")

		time.Sleep(3 * time.Second) // the timed save ends inside the bubble
		assert.Equal(t, "big", readState(t, filepath.Join(dir, ddb, FormatJSON.fileName())),
			"the timed save's finished snapshot is renamed, not discarded")
	})
}

// Test_Engine_ShutdownBesideAStuckTimedSync is review SQ-295 A1: the timed save
// renamed sqs v1 and is stuck in the s3 asset sync when the shutdown arrives,
// and sqs v2 was acknowledged after the rename. Stop saves sqs v2 beside the
// stuck save, within its own deadline, so no later flush is needed for it.
func Test_Engine_ShutdownBesideAStuckTimedSync(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSyncFixture(t, 10*time.Millisecond)
		sqs := &stateSnap{state: sqsV1}
		f.e.Register(sqsName, sqs)
		sqsFile := filepath.Join(filepath.Dir(filepath.Dir(f.stateFile)), sqsName, FormatJSON.fileName())

		started, release := gateFirstSync(f.a, nil)
		defer release()

		f.commit(t, "body")
		f.e.MarkDirty(sqsName)
		<-started // the timed save renamed sqs v1 and is stuck syncing

		assert.Equal(t, sqsV1, readState(t, sqsFile))

		sqs.set(sqsAcked)
		f.e.MarkDirty(sqsName)

		saveCtx, cancelSave := context.WithTimeout(context.WithoutCancel(t.Context()), 300*time.Millisecond)
		defer cancelSave()

		start := time.Now()
		err := f.e.Stop(saveCtx)
		require.ErrorIs(t, err, context.DeadlineExceeded, "the s3 save outlasts the save deadline")
		assert.Equal(t, 300*time.Millisecond, time.Since(start), "Stop returns at its own deadline")
		assert.Equal(t, sqsAcked, readState(t, sqsFile),
			"Stop saved sqs beside the stuck timed save")

		release()
		require.NoError(t, f.e.Close(t.Context()))
	})
}

// Test_Engine_FlushBesideKeepsALeaderWithItsHeldFollower checks that a flush
// beside a timed one saves neither half of a leader and follower pair the
// timed flush holds. The timed flush renamed dynamodb v1 and is inside the
// dynamodbstreams Snapshot; dynamodb v2 then comes. Saving dynamodb v2 beside
// it would put the table on disk ahead of a streams file taken after v1. The
// shutdown saves sqs at once, and the pair after the timed flush ends.
func Test_Engine_FlushBesideKeepsALeaderWithItsHeldFollower(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		e := New(quietConfig(dir, 10*time.Millisecond))
		table := &stateSnap{state: "table v1"}
		gate := make(chan struct{})
		streams := &stateSnap{state: "streams v1", gate: gate, entered: make(chan struct{})}
		sqs := &stateSnap{state: sqsV1}
		e.Register(ddb, table)
		e.Register(ddbStream, followingSnap{stateSnap: streams, leader: ddb})
		e.Register(sqsName, sqs)
		file := func(service string) string { return readState(t, filepath.Join(dir, service, FormatJSON.fileName())) }

		e.MarkDirty(ddb)
		<-streams.entered // the timed save renamed table v1 and is inside the streams Snapshot

		assert.Equal(t, "table v1", file(ddb))

		table.set("table v2")
		streams.set("streams v2")
		e.MarkDirty(ddb)
		e.MarkDirty(ddbStream)
		sqs.set("sqs v2")
		e.MarkDirty(sqsName)

		stopped := make(chan error)

		go func() { stopped <- e.Stop(t.Context()) }()

		synctest.Wait() // Stop saved what it could beside the timed save, and waits for it

		assert.Equal(t, "sqs v2", file(sqsName), "sqs is saved beside the timed save")
		assert.Equal(t, "table v1", file(ddb), "the leader of a held follower is not saved beside")

		close(gate)
		require.NoError(t, <-stopped)

		assert.Equal(t, "table v2", file(ddb))
		assert.Equal(t, "streams v2", file(ddbStream))
	})
}

// recLeader is a stateSnap Leader that logs its Snapshots and Followed calls.
type recLeader struct {
	*stateSnap

	log *orderLog
}

func (l recLeader) Snapshot(w io.Writer) error {
	l.log.add(ddb)

	return l.stateSnap.Snapshot(w)
}

func (l recLeader) Followed(saved bool) { l.log.add(fmt.Sprintf("followed %t", saved)) }

// recFollower is a stateSnap that follows dynamodb and logs each Snapshot as
// it returns.
type recFollower struct {
	*stateSnap

	log *orderLog
}

func (recFollower) Follows() string { return ddb }

func (f recFollower) Snapshot(w io.Writer) error {
	err := f.stateSnap.Snapshot(w)
	f.log.add(ddbStream)

	return err
}

// Test_Engine_FlushBesideIsNotUnfollowedByTheTimedFlushEnding checks that the
// timed flush tells its leader Followed(false) when the pair's saves end, not
// when the whole flush does. The timed flush saved the pair and is stuck in
// the s3 sync; a Flush beside it then saves the pair again. The timed flush
// ends between that Flush's table and streams snapshots, and a Followed(false)
// from it then would drop the cut the streams snapshot is to write.
func Test_Engine_FlushBesideIsNotUnfollowedByTheTimedFlushEnding(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSyncFixture(t, 10*time.Millisecond)
		log := &orderLog{}
		table := &stateSnap{state: "table v1"}
		streams := &stateSnap{state: "streams v1"}
		f.e.Register(ddb, recLeader{stateSnap: table, log: log})
		f.e.Register(ddbStream, recFollower{stateSnap: streams, log: log})

		started, release := gateFirstSync(f.a, nil)
		defer release()

		f.commit(t, "body")
		f.e.MarkDirty(ddb)
		<-started // the timed flush saved the pair and is stuck in the s3 sync

		gate := make(chan struct{})

		streams.mu.Lock()
		streams.gate, streams.entered = gate, make(chan struct{})
		streams.mu.Unlock()

		log.mu.Lock()
		log.seen = nil
		log.mu.Unlock()

		table.set("table v2")
		f.e.MarkDirty(ddb)

		flushed := make(chan error)

		go func() { flushed <- f.e.Flush(t.Context()) }()

		<-streams.entered // the Flush beside saved the table and is in the streams Snapshot

		release()
		synctest.Wait() // the timed flush has ended

		close(gate)
		require.NoError(t, <-flushed)

		assert.Equal(t, []string{"followed true", ddb, ddbStream, "followed false"}, log.seen,
			"the leader is told false only after its follower's snapshot")
	})
}

// Test_Engine_Close_PastItsDeadlineWritesNothing is why saver.save skips the
// engine's Close once the services' Close has run its deadline out: a flush
// under an ended context renames nothing, so skipping it costs no save.
func Test_Engine_Close_PastItsDeadlineWritesNothing(t *testing.T) {
	dir := t.TempDir()
	e := New(quietConfig(dir, time.Hour))
	sqs := &stateSnap{state: sqsV1}
	e.Register(sqsName, sqs)
	sqsFile := filepath.Join(dir, sqsName, FormatJSON.fileName())

	e.MarkDirty(sqsName)
	require.NoError(t, e.Flush(t.Context()))

	sqs.set("sqs v2")
	e.MarkDirty(sqsName)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.ErrorIs(t, e.Close(ctx), context.Canceled)
	assert.Equal(t, sqsV1, readState(t, sqsFile))
}
