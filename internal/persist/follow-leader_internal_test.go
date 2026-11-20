package persist

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The rule these tests hold (SQ-367, SQ-371): a [Leader] is told before each
// Snapshot whether a flush saves a follower right after it, and a flush that
// told it true tells it false once the flush ends, whatever happened.

// What telling logs for Followed.
const (
	followedTrue  = "followed true"
	followedFalse = "followed false"
)

// telling is flaky with Followed, which it logs as "followed <saved>".
type telling struct{ flaky }

func (l telling) Followed(saved bool) { l.log.add(fmt.Sprintf("followed %t", saved)) }

// newTellingPair is newPair whose leader is a Leader, with its follower
// registered or not.
//
//nolint:revive // flag-parameter: withFollower is whether the follower is persisted, the case under test.
func newTellingPair(t *testing.T, withFollower bool) *pair {
	t.Helper()

	p := &pair{dir: t.TempDir(), log: &orderLog{}, leaderFails: &atomic.Bool{}, followerFails: &atomic.Bool{}}
	p.e = New(quietConfig(p.dir, time.Hour))
	p.e.Register(leader, telling{flaky{logged{name: leader, log: p.log}, p.leaderFails}})

	if withFollower {
		p.e.Register(follower, flakyFollower{flaky{logged{name: follower, leader: leader, log: p.log}, p.followerFails}})
		p.followerState = p.e.services[follower]
	}

	p.leaderState = p.e.services[leader]

	return p
}

func Test_Engine_Flush_LeaderToldItsFollowerIsSaved(t *testing.T) {
	p := newTellingPair(t, true)

	p.e.MarkDirty(leader)
	require.NoError(t, p.e.Flush(t.Context()))
	require.Equal(t,
		[]string{followedTrue, leader, "commit " + leader, follower, "commit " + follower, followedFalse}, p.saves(),
		"the flush ends by telling the leader no follower is saved after it any more")
}

func Test_Engine_Flush_LeaderWithNoFollowerToldSo(t *testing.T) {
	p := newTellingPair(t, false)

	p.e.MarkDirty(leader)
	require.NoError(t, p.e.Flush(t.Context()))
	require.Equal(t, []string{followedFalse, leader, "commit " + leader}, p.saves(),
		"a follower that is not persisted saves nothing from the leader's snapshot")
}

// Test_Engine_TimerFlush_LeaderSavedAloneToldSo: the follower is in its own
// backoff, so the timer's flush saves the leader alone.
func Test_Engine_TimerFlush_LeaderSavedAloneToldSo(t *testing.T) {
	p := newTellingPair(t, true)

	p.followerFails.Store(true)
	p.e.MarkDirty(leader)
	require.ErrorIs(t, p.e.Flush(t.Context()), errTestDisk)
	p.saves()
	p.followerFails.Store(false)

	p.e.MarkDirty(leader)
	_, err := p.e.writeDirty(t.Context(), byTimer)
	require.NoError(t, err)
	require.Equal(t, []string{followedFalse, leader, "commit " + leader}, p.saves())
}

func Test_Engine_Flush_FailedLeaderToldNoFollowerIsSaved(t *testing.T) {
	p := newTellingPair(t, true)

	p.leaderFails.Store(true)
	p.e.MarkDirty(leader)
	require.ErrorIs(t, p.e.Flush(t.Context()), errTestDisk)
	require.Equal(t, []string{followedTrue, leader, followedFalse}, p.saves(),
		"the follower gives way, so nothing is saved from the failed snapshot")
}

// Test_Engine_Flush_FollowerNeverSnapshottedLeaderToldAtTheEnd: SQ-370 S2.
// The leader commits, then the follower's save fails before its Snapshot, its
// directory unmakeable. Nothing reads what the leader kept for it, so the
// flush ends by telling the leader so.
func Test_Engine_Flush_FollowerNeverSnapshottedLeaderToldAtTheEnd(t *testing.T) {
	p := newTellingPair(t, true)
	require.NoError(t, os.WriteFile(filepath.Join(p.dir, follower), []byte("in the way"), 0o600))

	p.e.MarkDirty(leader)
	require.Error(t, p.e.Flush(t.Context()))
	require.Equal(t, []string{followedTrue, leader, "commit " + leader, followedFalse}, p.saves())
}

// cancelling is telling whose Committed cancels the flush, between the
// leader's save and its follower's.
type cancelling struct {
	telling

	cancel context.CancelFunc
}

func (c cancelling) Committed() {
	c.telling.Committed()
	c.cancel()
}

// Test_Engine_Flush_CancelledBetweenTheSavesLeaderToldAtTheEnd: SQ-370 S2.
// The flush ends between the leader's commit and its follower's save.
func Test_Engine_Flush_CancelledBetweenTheSavesLeaderToldAtTheEnd(t *testing.T) {
	dir := t.TempDir()
	log := &orderLog{}
	ctx, cancel := context.WithCancel(t.Context())
	e := New(quietConfig(dir, time.Hour))
	e.Register(leader, cancelling{telling{flaky{logged{name: leader, log: log}, &atomic.Bool{}}}, cancel})
	e.Register(follower, flakyFollower{flaky{logged{name: follower, leader: leader, log: log}, &atomic.Bool{}}})

	e.MarkDirty(leader)
	require.ErrorIs(t, e.Flush(ctx), context.Canceled)
	require.Equal(t, []string{followedTrue, leader, "commit " + leader, followedFalse}, log.seen)
}

// tellingAssetLeader is assetLeader with Followed, and a hook after Snapshot.
type tellingAssetLeader struct {
	assetLeader

	after func()
}

func (a tellingAssetLeader) Snapshot(w io.Writer) error {
	err := a.assetLeader.Snapshot(w)
	a.after()

	return err
}

func (a tellingAssetLeader) Followed(saved bool) { a.log.add(fmt.Sprintf("followed %t", saved)) }

// Test_Engine_Flush_DeferredLeaderAbandonedToldAtTheEnd: SQ-370 N1. A leader
// with assets to sync is deferred to the second pass, and the flush ends
// before it: neither is saved, and the leader is told no follower is.
func Test_Engine_Flush_DeferredLeaderAbandonedToldAtTheEnd(t *testing.T) {
	dir := t.TempDir()
	log := &orderLog{}
	ctx, cancel := context.WithCancel(t.Context())
	e := New(quietConfig(dir, time.Hour))
	n := &namer{}
	e.Register(leader, tellingAssetLeader{assetLeader{n, log}, cancel})
	e.Register(follower, flakyFollower{flaky{logged{name: follower, leader: leader, log: log}, &atomic.Bool{}}})

	a, err := e.Assets(leader)
	require.NoError(t, err)

	n.add(commitAsset(t, a, "body"))
	e.MarkDirty(leader)
	require.ErrorIs(t, e.Flush(ctx), context.Canceled)
	require.Equal(t, []string{followedTrue, leader, followedFalse}, log.seen)
	require.Empty(t, stateFiles(t, dir))
}
