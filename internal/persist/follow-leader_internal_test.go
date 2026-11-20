package persist

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The rule these tests hold (SQ-367): a [Leader] is told before each Snapshot
// whether a flush saves a follower right after it, and told no follower is
// saved when its save fails.

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
	require.Equal(t, []string{followedTrue, leader, "commit " + leader, follower, "commit " + follower}, p.saves())
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
