package persist

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests of the rulings of review SQ-391 on the branch of SQ-384: a service a
// Flush moved from the timed save and did not reach, the Flush that comes while
// a Load waits, and what a panic of a leader leaves held.

const (
	// athenaState and bSlowState are the states of the slow services of these tests.
	athenaState = "athena state"
	bSlowState  = "b-slow state"
)

// fileOf is the JSON state file of a service.
func fileOf(dir, service string) string { return filepath.Join(dir, service, FormatJSON.fileName()) }

// onDisk reports whether a service has a state file.
func onDisk(dir, service string) bool {
	_, err := os.Stat(fileOf(dir, service))

	return err == nil
}

// held is what keeps a guard of the engine: the flushes counted, the services
// a flush still holds, and whether a timer is armed.
type held struct {
	flushing, services, sem, sharers, waiting int
	armed                                     bool
}

func heldBy(e *Engine) held {
	e.mu.Lock()
	defer e.mu.Unlock()

	h := held{
		flushing: e.flushing, sem: len(e.sem), sharers: e.sharers, waiting: e.waiting, armed: e.timer != nil,
	}

	for _, t := range e.services {
		if t.owner != nil {
			h.services++
		}
	}

	return h
}

// Test_Engine_ShutdownSavesAMovedServiceBesideAnUnmeasuredSlowOne is review
// SQ-391 C3 and C4. The timed save took athena, which sorts first and whose
// Snapshot takes timed, and sqs, acknowledged and queued behind it. The
// shutdown is saver.save's: Stop under 300 ms, Close under 700 ms, the kill at
// 800 ms, and b-slow, dirty and never measured, takes a second to snapshot. On
// main the timed save writes sqs when athena's Snapshot ends. Stop's flush
// moves sqs to itself, and so saves it ahead of b-slow, which a service it did
// not take sorts after: sqs is on disk before the timed save ends, and the
// kill finds it there.
func Test_Engine_ShutdownSavesAMovedServiceBesideAnUnmeasuredSlowOne(t *testing.T) {
	for name, measured := range map[string]time.Duration{
		"sqs was never measured":        0,
		"sqs measured at a millisecond": time.Millisecond,
	} {
		t.Run(name, func(t *testing.T) { shutdownSavesAMoved(t, measured) })
	}
}

func shutdownSavesAMoved(t *testing.T, measured time.Duration) {
	t.Helper()

	for name, timed := range map[string]time.Duration{
		"the timed save ends inside the save deadline": 200 * time.Millisecond,
		"the timed save ends after the save deadline":  500 * time.Millisecond,
		"the timed save ends long after the kill":      10 * time.Second,
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				dir := t.TempDir()
				e := New(quietConfig(dir, 10*time.Millisecond))
				big := verySlow{stateSnap: &stateSnap{state: athenaState}, d: timed, started: make(chan struct{})}
				mine := verySlow{stateSnap: &stateSnap{state: bSlowState}, d: time.Second, started: make(chan struct{})}
				e.Register(slowName, big)
				e.Register("b-slow", mine)
				e.Register(sqsName, &stateSnap{state: sqsAcked})

				e.mu.Lock()
				e.services[sqsName].took = measured
				e.mu.Unlock()

				e.MarkDirty(sqsName)
				e.MarkDirty(slowName)
				<-big.started
				e.MarkDirty("b-slow")

				saveCtx, cancelSave := context.WithTimeout(context.WithoutCancel(t.Context()), 300*time.Millisecond)
				defer cancelSave()

				closeCtx, cancelClose := context.WithTimeout(context.WithoutCancel(t.Context()), 700*time.Millisecond)
				defer cancelClose()

				done := make(chan struct{})

				go func() {
					defer close(done)

					_ = e.Stop(saveCtx)

					if closeCtx.Err() == nil {
						_ = e.Close(closeCtx)
					}
				}()

				time.Sleep(50 * time.Millisecond)
				assert.True(t, onDisk(dir, sqsName), "sqs is on disk long before the kill, ahead of b-slow's Snapshot")
				time.Sleep(750 * time.Millisecond)
				assert.True(t, onDisk(dir, sqsName), "sqs, acknowledged before the shutdown, is on disk at the kill")

				<-done
				time.Sleep(15 * time.Second)
				synctest.Wait()
			})
		})
	}
}

// movedBehindSlowTimedSave is the setup of the tests of what a Flush that
// moved a queued service does when its deadline ends before it reaches it. The
// timed save took athena, with a Snapshot of timed, and then x-slow, a second
// long and measured at a millisecond, and sqs, measured at a second. A Flush
// under 300 ms moves both and saves x-slow first, since a service it moved
// sorts by its measured time among those it moved; its deadline ends inside
// x-slow's Snapshot, and it abandons sqs when that Snapshot returns, at a
// second.
func movedBehindSlowTimedSave(t *testing.T, timed time.Duration) (e *Engine, sqs *countingSnap, dir string) {
	t.Helper()

	dir = t.TempDir()
	e = New(quietConfig(dir, 10*time.Millisecond))
	big := verySlow{stateSnap: &stateSnap{state: athenaState}, d: timed, started: make(chan struct{})}
	mine := verySlow{stateSnap: &stateSnap{state: bSlowState}, d: time.Second, started: make(chan struct{})}
	sqs = &countingSnap{stateSnap: &stateSnap{state: sqsAcked}}
	e.Register(slowName, big)
	e.Register("x-slow", mine)
	e.Register(sqsName, sqs)

	e.mu.Lock()
	e.services["x-slow"].took = time.Millisecond
	e.services[sqsName].took = time.Second
	e.mu.Unlock()

	e.MarkDirty(sqsName)
	e.MarkDirty("x-slow")
	e.MarkDirty(slowName)
	<-big.started

	return e, sqs, dir
}

// Test_Engine_FlushDoesNotCountAMovedServiceItDidNotReachAsFailed is review
// SQ-391 C2 and C3 in one: the Flush moved sqs from the timed save, and its
// deadline ends inside x-slow's Snapshot, before it reaches sqs. sqs was never
// tried, so it is no failure and has no backoff, whether the timed save has
// ended (200 ms) or is still inside athena's Snapshot, which the Flush's
// deadline then ends (3 s). It is dirty, and the next Flush writes it, once.
func Test_Engine_FlushDoesNotCountAMovedServiceItDidNotReachAsFailed(t *testing.T) {
	for name, timed := range map[string]time.Duration{
		"the timed save has ended":      200 * time.Millisecond,
		"the timed save is still under": 3 * time.Second,
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e, sqs, dir := movedBehindSlowTimedSave(t, timed)

				ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 300*time.Millisecond)
				defer cancel()

				require.ErrorIs(t, e.Stop(ctx), context.DeadlineExceeded, "the Flush runs out of time inside x-slow's Snapshot")
				time.Sleep(timed + time.Second)
				synctest.Wait() // the timed save is over too
				assert.False(t, onDisk(dir, sqsName), "sqs was not written by either")

				e.mu.Lock()
				ts := e.services[sqsName]
				assert.Zero(t, ts.failures, "a moved service the Flush did not reach is no failure")
				assert.True(t, ts.retryAt.IsZero(), "and has no backoff")
				assert.True(t, ts.dirty)
				assert.Nil(t, ts.owner)
				e.mu.Unlock()

				assert.Equal(t, held{flushing: 0}, heldBy(e), "nothing is held")

				require.NoError(t, e.Flush(t.Context()))
				assert.Equal(t, sqsAcked, readState(t, fileOf(dir, sqsName)))
				assert.Equal(t, 1, sqs.count(), "sqs is snapshotted once")
			})
		})
	}
}

// Test_Engine_FlushDoesNotCountAMovedPairItDidNotReachAsFailed: the Flush moved
// the dynamodb pair, and its deadline ends before it reaches either. Neither
// is a failure, and the next Flush saves them as a pair, once.
func Test_Engine_FlushDoesNotCountAMovedPairItDidNotReachAsFailed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		e := New(quietConfig(dir, 10*time.Millisecond))
		log := &orderLog{}
		big := verySlow{stateSnap: &stateSnap{state: athenaState}, d: 3 * time.Second, started: make(chan struct{})}
		mine := verySlow{stateSnap: &stateSnap{state: bSlowState}, d: time.Second, started: make(chan struct{})}
		e.Register(slowName, big)
		e.Register("x-slow", mine)
		e.Register(ddb, recLeader{stateSnap: &stateSnap{state: "table v2"}, log: log})
		e.Register(ddbStream, recFollower{stateSnap: &stateSnap{state: "streams v2"}, log: log})

		e.mu.Lock()
		e.services["x-slow"].took = time.Millisecond
		e.services[ddb].took = time.Second
		e.mu.Unlock()

		e.MarkDirty(ddb)
		e.MarkDirty("x-slow")
		e.MarkDirty(slowName)
		<-big.started

		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 300*time.Millisecond)
		defer cancel()

		require.ErrorIs(t, e.Stop(ctx), context.DeadlineExceeded)
		time.Sleep(4 * time.Second)
		synctest.Wait()
		assert.False(t, onDisk(dir, ddb) || onDisk(dir, ddbStream), "neither was written")

		e.mu.Lock()
		for _, name := range []string{ddb, ddbStream} {
			assert.Zero(t, e.services[name].failures, name)
			assert.True(t, e.services[name].retryAt.IsZero(), name)
			assert.True(t, e.services[name].dirty, name)
		}
		e.mu.Unlock()

		require.NoError(t, e.Flush(t.Context()))

		log.mu.Lock()
		defer log.mu.Unlock()

		assert.Equal(t, []string{followedTrue, ddb, ddbStream, followedFalse}, log.seen,
			"the pair is saved once, by the Flush")
	})
}

// Test_Engine_FlushWaitsBehindAWaitingLoad is review SQ-391 B: the timed save
// holds sqs v2 behind a slow Snapshot, and v1 is on disk. A Load is called, and
// preempts the timed save and waits. A Flush that comes then waits behind the
// Load, as on main, and does not write v2 ahead of it: the Load restores the
// v1 that was on disk when it was called.
func Test_Engine_FlushWaitsBehindAWaitingLoad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		e := New(quietConfig(dir, 10*time.Millisecond))
		big := verySlow{stateSnap: &stateSnap{state: athenaState}, d: 2 * time.Second, started: make(chan struct{})}
		sqs := &stateSnap{state: sqsV1}
		e.Register(slowName, big)
		e.Register(sqsName, sqs)
		e.MarkDirty(sqsName)
		require.NoError(t, e.Flush(t.Context()))

		sqs.set("sqs v2")
		e.MarkDirty(sqsName)
		e.MarkDirty(slowName)
		<-big.started // the timed save took both and is inside athena

		loaded := make(chan error, 1)

		go func() { loaded <- e.Load(t.Context()) }()

		synctest.Wait() // the Load preempted the timed save and waits

		flushed := make(chan error, 1)

		go func() { flushed <- e.Flush(t.Context()) }()

		synctest.Wait()
		assert.Equal(t, sqsV1, readState(t, fileOf(dir, sqsName)), "the Flush has written nothing while the Load waits")
		assert.Equal(t, held{flushing: 1, services: 2, sem: 1, sharers: 1, waiting: 2}, heldBy(e),
			"the Flush waits for sem, not shares it")

		time.Sleep(3 * time.Second)
		synctest.Wait()
		require.NoError(t, <-loaded)
		require.NoError(t, <-flushed)

		sqs.mu.Lock()
		restored := sqs.restored
		sqs.mu.Unlock()

		assert.Equal(t, sqsV1, restored, "the Load restores what was on disk when it was called")
		assert.Equal(t, held{}, heldBy(e))
		require.NoError(t, e.Close(t.Context()))
	})
}

// followedPanics is a stateSnap Leader whose Followed panics for both values.
type followedPanics struct{ *stateSnap }

func (followedPanics) Followed(bool) { panic("followed blew up") }

// Test_Engine_TimedFlush_FollowedThatAlwaysPanicsFreesTheFlush is review
// SQ-391 E: a Leader whose Followed panics for false as well as true, in the
// timed flush. The panic that ends the flush calls Followed(false) in
// interrupt, which panics again; the flush is still freed: no count of flushes,
// no service held, and the timer saves another service later.
func Test_Engine_TimedFlush_FollowedThatAlwaysPanicsFreesTheFlush(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		e := New(quietConfig(dir, 10*time.Millisecond))
		sqs := &stateSnap{state: sqsV1}
		e.Register(ddb, followedPanics{&stateSnap{state: "tables"}})
		e.Register(ddbStream, followingSnap{stateSnap: &stateSnap{state: "stream records"}, leader: ddb})
		e.Register(sqsName, sqs)

		e.MarkDirty(ddb)
		time.Sleep(50 * time.Millisecond)
		synctest.Wait()

		h := heldBy(e)
		assert.Zero(t, h.flushing, "the panicked flush is over")
		assert.Zero(t, h.services, "it holds no service")
		assert.Zero(t, h.sem)

		sqs.set("sqs v2")
		e.MarkDirty(sqsName)
		time.Sleep(time.Hour)
		synctest.Wait()

		assert.Equal(t, "sqs v2", readState(t, fileOf(dir, sqsName)), "the timer still saves the other services")
	})
}

// panickedLeader is a Leader whose Snapshot panics inside its lock, which it
// leaves held. Its Followed would wait for the lock, so it counts the calls
// that find it held.
type panickedLeader struct {
	mu    sync.Mutex
	stuck atomic.Int32
}

func (l *panickedLeader) Snapshot(io.Writer) error {
	l.mu.Lock()

	panic("snapshot blew up inside the lock")
}

func (*panickedLeader) Restore(io.Reader) error { return nil }

func (l *panickedLeader) Followed(bool) {
	if !l.mu.TryLock() {
		l.stuck.Add(1)

		return
	}

	l.mu.Unlock()
}

// Test_Engine_Flush_NeverCallsALeaderWhoseSnapshotPanicked is review SQ-391 H:
// callSnapshot says the engine never calls a service again after its Snapshot
// panicked, since the panic may have left a lock held. That includes the
// Followed(false) that ends the flush.
func Test_Engine_Flush_NeverCallsALeaderWhoseSnapshotPanicked(t *testing.T) {
	e := New(quietConfig(t.TempDir(), time.Hour))
	l := &panickedLeader{}
	e.Register(ddb, l)
	e.Register(ddbStream, followingSnap{stateSnap: &stateSnap{state: "stream records"}, leader: ddb})
	e.MarkDirty(ddb)

	require.ErrorIs(t, e.Flush(t.Context()), ErrUnsafe, "the leader is off")
	assert.Zero(t, l.stuck.Load(), "no call found the leader's lock held")
	assert.Equal(t, held{}, heldBy(e))
}

// Test_Engine_TimedFlush_ALentDeadlineStopsTheSaveBehindAJoinedFlush: the timed
// flush saves a, whose Snapshot is held, and then b, which follows a, so a
// Flush cannot save it beside the timed save. The Flush joins, has nothing to
// take, and waits for the timed save to end; its context is cancelled, and
// lends the timed save that end. a, whose Snapshot is done, is renamed, and b
// is not saved: it gave way, with no failure and no backoff. Without the lend
// the timed save goes on and writes b.
func Test_Engine_TimedFlush_ALentDeadlineStopsTheSaveBehindAJoinedFlush(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		e := New(quietConfig(dir, 10*time.Second))
		gate := make(chan struct{})

		a := &stateSnap{state: "a", entered: make(chan struct{}, 1), gate: gate}
		e.Register("a", a)
		e.Register("b", followingSnap{stateSnap: &stateSnap{state: "b"}, leader: "a"})
		e.MarkDirty("a")
		e.MarkDirty("b")

		<-a.entered // the timed flush, at 10 s, is inside a's Snapshot

		ctx, cancel := context.WithCancel(t.Context())
		finished := make(chan error, 1)

		go func() { finished <- e.Flush(ctx) }()

		synctest.Wait()
		cancel() // the Flush's caller goes away, and lends the timed flush that
		require.Error(t, <-finished)
		close(gate)
		synctest.Wait()

		assert.Equal(t, "a", readState(t, fileOf(dir, "a")), "a, whose Snapshot was done, is saved")
		assert.False(t, onDisk(dir, "b"), "b is not: the timed flush ended with the Flush's context")

		e.mu.Lock()
		tb := e.services["b"]
		assert.Zero(t, tb.failures)
		assert.True(t, tb.retryAt.IsZero())
		assert.True(t, tb.dirty)
		e.mu.Unlock()

		time.Sleep(10*time.Second + time.Millisecond)
		synctest.Wait()
		assert.Equal(t, "b", readState(t, fileOf(dir, "b")), "the next timed flush saves it")
		require.NoError(t, e.Close(t.Context()))
	})
}
