package persist

import (
	"errors"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The rule these tests hold (SQ-365, from the SQ-363 probes): a follower is
// saved in a flush only right after its leader's snapshot was renamed in that
// flush. A leader that is skipped or fails leaves its follower unsaved and
// dirty; a follower in its own backoff leaves its leader to be saved alone.

var errTestDisk = errors.New("test: disk")

// flaky is logged whose Snapshot fails while fail is set. It records each
// commit the engine reports as "commit <name>".
type flaky struct {
	logged

	fail *atomic.Bool
}

func (f flaky) Snapshot(w io.Writer) error {
	f.log.add(f.name)

	if f.fail.Load() {
		return errTestDisk
	}

	_, err := io.WriteString(w, f.name)

	return err //nolint:wrapcheck // a test double
}

func (f flaky) Committed() { f.log.add("commit " + f.name) }

// flakyFollower is flaky with Follows.
type flakyFollower struct{ flaky }

func (f flakyFollower) Follows() string { return f.leader }

// stateFiles are the services with a store.json under dir.
func stateFiles(t *testing.T, dir string) []string {
	t.Helper()

	got, err := filepath.Glob(filepath.Join(dir, "*", "store.json"))
	require.NoError(t, err)

	for i := range got {
		got[i] = filepath.Base(filepath.Dir(got[i]))
	}

	return got
}

// pair is a leader and its follower, each failing while its flag is set.
type pair struct {
	e                          *Engine
	dir                        string
	log                        *orderLog
	leaderFails, followerFails *atomic.Bool
	leaderState, followerState *tracked
}

func newPair(t *testing.T, freq time.Duration) *pair {
	t.Helper()

	p := &pair{dir: t.TempDir(), log: &orderLog{}, leaderFails: &atomic.Bool{}, followerFails: &atomic.Bool{}}
	p.e = New(quietConfig(p.dir, freq))
	p.e.Register(follower, flakyFollower{flaky{logged{name: follower, leader: leader, log: p.log}, p.followerFails}})
	p.e.Register(leader, flaky{logged{name: leader, log: p.log}, p.leaderFails})
	p.e.Register(other, logged{name: other, log: p.log})

	p.leaderState, p.followerState = p.e.services[leader], p.e.services[follower]

	return p
}

// saves returns the saves logged since the last call, and clears them.
func (p *pair) saves() []string {
	p.log.mu.Lock()
	defer p.log.mu.Unlock()

	seen := p.log.seen
	p.log.seen = nil

	return seen
}

func (p *pair) followerDirty() (dirty bool, failures int) {
	p.e.mu.Lock()
	defer p.e.mu.Unlock()

	return p.followerState.dirty, p.followerState.failures
}

// Test_Engine_Flush_NoFollowerAfterAFailedLeader: SQ-363 row D. The leader's
// save fails after it was taken; its follower is not saved, so the streams
// file that matched the table file on disk is not replaced by one of a cut the
// disk never got. The follower stays dirty with no failure counted, and the
// next flush that saves the leader saves it right after.
func Test_Engine_Flush_NoFollowerAfterAFailedLeader(t *testing.T) {
	p := newPair(t, time.Hour)

	p.leaderFails.Store(true)
	p.e.MarkDirty(leader)
	require.ErrorIs(t, p.e.Flush(t.Context()), errTestDisk)
	require.Equal(t, []string{leader}, p.saves(), "the follower is not saved after a failed leader")
	require.Empty(t, stateFiles(t, p.dir))

	dirty, failures := p.followerDirty()
	require.True(t, dirty, "the follower stays dirty")
	require.Zero(t, failures, "a follower that gave way did not fail")

	p.leaderFails.Store(false)
	require.NoError(t, p.e.Flush(t.Context()))
	require.Equal(t, []string{leader, "commit " + leader, follower, "commit " + follower}, p.saves())
	require.ElementsMatch(t, []string{leader, follower}, stateFiles(t, p.dir))
}

// Test_Engine_TimerFlush_FollowerWaitsOutItsLeadersBackoff: SQ-363 row G. The
// leader is in its backoff and another service is due: the timer's flush
// saves the other service, and the follower waits for its leader.
func Test_Engine_TimerFlush_FollowerWaitsOutItsLeadersBackoff(t *testing.T) {
	p := newPair(t, time.Hour)

	p.leaderFails.Store(true)
	p.e.MarkDirty(leader)
	require.ErrorIs(t, p.e.Flush(t.Context()), errTestDisk)
	p.saves()
	p.leaderFails.Store(false)

	p.e.MarkDirty(other)
	_, err := p.e.writeDirty(t.Context(), byTimer)
	require.NoError(t, err)
	require.Equal(t, []string{other}, p.saves(), "the follower is not saved while its leader backs off")

	dirty, _ := p.followerDirty()
	require.True(t, dirty)
}

// Test_Engine_TimerFlush_LeaderSavedAloneWhileItsFollowerBacksOff: SQ-363 row
// H, as ruled. The follower's save failed, so it is in its backoff: the
// timer's flush saves the dirty leader alone, and the follower's file is the
// older one until the follower is due. Then the dirty follower takes its clean
// leader with it.
func Test_Engine_TimerFlush_LeaderSavedAloneWhileItsFollowerBacksOff(t *testing.T) {
	p := newPair(t, time.Hour)

	p.followerFails.Store(true)
	p.e.MarkDirty(leader)
	require.ErrorIs(t, p.e.Flush(t.Context()), errTestDisk)
	p.saves()
	p.followerFails.Store(false)

	p.e.MarkDirty(leader)
	_, err := p.e.writeDirty(t.Context(), byTimer)
	require.NoError(t, err)
	require.Equal(t, []string{leader, "commit " + leader}, p.saves(), "the leader is saved alone")

	dirty, failures := p.followerDirty()
	require.True(t, dirty)
	require.Equal(t, 1, failures)

	p.e.mu.Lock()
	p.followerState.retryAt = time.Now().Add(-time.Second)
	p.e.mu.Unlock()

	_, err = p.e.writeDirty(t.Context(), byTimer)
	require.NoError(t, err)
	require.Equal(t, []string{leader, "commit " + leader, follower, "commit " + follower}, p.saves(),
		"the due follower takes its clean leader with it")
}

// Test_Engine_nextDue_FollowerNoSoonerThanItsLeader: at PERSIST_FREQUENCY 0
// a dirty follower whose leader backs off would fire the timer for flushes
// that take nothing. It is due when its leader is, and never when its leader
// is not persisted.
func Test_Engine_nextDue_FollowerNoSoonerThanItsLeader(t *testing.T) {
	p := newPair(t, 0)
	now := time.Now()
	retryAt := now.Add(time.Hour)

	p.e.mu.Lock()
	p.leaderState.dirty, p.leaderState.retryAt = true, retryAt
	p.followerState.dirty = true
	at := p.e.nextDueLocked(now)
	p.e.mu.Unlock()

	require.True(t, at.Equal(retryAt), "due at %v, want the leader's retryAt %v", at, retryAt)

	alone := New(quietConfig(t.TempDir(), 0))
	alone.Register(follower, flakyFollower{flaky{logged{name: follower, leader: leader, log: p.log}, p.followerFails}})
	alone.MarkDirty(follower)

	alone.mu.Lock()
	at = alone.nextDueLocked(now)
	alone.mu.Unlock()

	require.True(t, at.IsZero(), "a follower whose leader is not persisted is never due")
}

// assetLeader is a leader with assets, logging its saves and commits.
type assetLeader struct {
	*namer

	log *orderLog
}

func (a assetLeader) Snapshot(w io.Writer) error {
	a.log.add(leader)

	return a.namer.Snapshot(w)
}

func (a assetLeader) Committed() { a.log.add("commit " + leader) }

// Test_Engine_Flush_FollowerBehindADeferredLeader: a leader whose assets need
// a sync is renamed in the second pass; its follower's snapshot is taken only
// after that rename, not in the first pass.
func Test_Engine_Flush_FollowerBehindADeferredLeader(t *testing.T) {
	dir := t.TempDir()
	log := &orderLog{}
	e := New(quietConfig(dir, time.Hour))
	n := &namer{}
	e.Register(leader, assetLeader{n, log})
	e.Register(follower, flakyFollower{flaky{logged{name: follower, leader: leader, log: log}, &atomic.Bool{}}})

	a, err := e.Assets(leader)
	require.NoError(t, err)

	n.add(commitAsset(t, a, "body"))
	e.MarkDirty(leader)
	require.NoError(t, e.Flush(t.Context()))
	require.Equal(t, []string{leader, "commit " + leader, follower, "commit " + follower}, log.seen)
	require.ElementsMatch(t, []string{leader, follower}, stateFiles(t, dir))
}
