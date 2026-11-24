package persist

import (
	"log/slog"
	"maps"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pendingSets is what an Assets records of its ids for the next save.
type pendingSets struct {
	committed, fresh, unsynced map[string]struct{}
}

func pendingSetsOf(a *Assets) pendingSets {
	a.mu.Lock()
	defer a.mu.Unlock()

	return pendingSets{
		committed: maps.Clone(a.committed),
		fresh:     maps.Clone(a.fresh),
		unsynced:  maps.Clone(a.unsynced),
	}
}

// Test_PendAgain_LeavesAssetsAsCommitLeftThem pins the benchmark hook PendAgain
// to commit: after a save, it puts the ids back in the sets commit left them
// in, and the next save syncs them as the first did and leaves the sets as the
// first left them. It runs once per sync path the platform has.
func Test_PendAgain_LeavesAssetsAsCommitLeftThem(t *testing.T) {
	for name, force := range syncModes {
		t.Run(name, func(t *testing.T) { pendAgainLeavesAssetsAsCommitLeftThem(t, force) })
	}
}

func pendAgainLeavesAssetsAsCommitLeftThem(t *testing.T, force func(*testing.T, *Assets)) {
	e := New(Config{
		PersistVarSet: true, BaseDir: t.TempDir(), Frequency: time.Hour,
		Logger: slog.New(slog.DiscardHandler),
	})
	t.Cleanup(func() { _ = e.Close(t.Context()) })

	n := &namer{}
	e.Register(syncService, n)

	a, err := e.Assets(syncService)
	require.NoError(t, err)
	force(t, a)

	var syncs atomic.Int64

	realSync := a.fsync
	a.fsync = func(f *os.File) error {
		syncs.Add(1)

		return realSync(f)
	}

	ids := []string{commitAsset(t, a, "one"), commitAsset(t, a, "two")}
	for _, id := range ids {
		n.add(id)
	}

	committed := pendingSetsOf(a)
	require.Len(t, committed.unsynced, len(ids))

	e.MarkDirty(syncService)
	require.NoError(t, e.Flush(t.Context()))

	saved := pendingSetsOf(a)
	firstSyncs := syncs.Swap(0)
	require.Positive(t, firstSyncs)
	require.NotEqual(t, committed, saved, "a save consumes what commit records")

	PendAgain(a, ids)
	assert.Equal(t, committed, pendingSetsOf(a))

	e.MarkDirty(syncService)
	require.NoError(t, e.Flush(t.Context()))

	assert.Equal(t, saved, pendingSetsOf(a))
	assert.Equal(t, firstSyncs, syncs.Load())
}
