//go:build darwin

package persist

import (
	"io/fs"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// syncModes is the one sync path macOS has: an fsync of each asset, then one
// flush of the drive. The assets take it as chooseSync made it, so every test
// that runs under syncModes runs the real fsync and the real F_FULLFSYNC.
var syncModes = map[string]func(*testing.T, *Assets){
	syncByFile: func(*testing.T, *Assets) {},
}

// syncEvents records the order of the syncs of one Assets, and the state file
// on disk at each flush.
type syncEvents struct {
	mu      sync.Mutex
	events  []string
	flushed []bool
}

// record replaces the fsync and the flush of a with ones that record the call
// and then run the real one; failFlush, when not nil, replaces the real flush.
// stateFile is checked at each flush.
func record(a *Assets, stateFile string, failFlush error) *syncEvents {
	s := &syncEvents{}

	realSync, realFlush := a.fsync, a.flush

	a.fsync = func(f *os.File) error {
		s.mu.Lock()
		s.events = append(s.events, "fsync")
		s.mu.Unlock()

		return realSync(f)
	}

	a.flush = func(f *os.File) error {
		_, err := os.Stat(stateFile)

		s.mu.Lock()
		s.events = append(s.events, "flush")
		s.flushed = append(s.flushed, err == nil)
		s.mu.Unlock()

		if failFlush != nil {
			return failFlush
		}

		return realFlush(f)
	}

	return s
}

func (s *syncEvents) count(event string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(slices.DeleteFunc(slices.Clone(s.events), func(e string) bool { return e != event }))
}

func (s *syncEvents) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.events[len(s.events)-1]
}

// Test_Assets_DarwinFlushesOncePerSave checks that a save syncs each pending
// asset and directory with fsync alone, then runs the drive flush once, after
// every one of them and before it renames the snapshot, and that a save with
// nothing pending runs no flush. The real F_FULLFSYNC runs on the assets
// directory, so an error from it would fail the save.
func Test_Assets_DarwinFlushesOncePerSave(t *testing.T) {
	f := newSyncFixture(t, time.Hour)
	events := record(f.a, f.stateFile, nil)

	for _, body := range []string{"one", "two", "three"} {
		f.commit(t, body)
	}

	require.NoError(t, f.e.Flush(t.Context()))
	assert.Equal(t, 1, events.count("flush"), "one flush for the three assets")
	assert.GreaterOrEqual(t, events.count("fsync"), 4, "each asset, and its directory")
	assert.Equal(t, "flush", events.last(), "the flush follows every fsync")
	assert.Equal(t, []bool{false}, events.flushed, "the snapshot is not renamed before the flush")
	assert.Len(t, f.onDisk(t), 3)

	f.e.MarkDirty(syncService)
	require.NoError(t, f.e.Flush(t.Context()))
	assert.Equal(t, 1, events.count("flush"), "a save with no pending asset flushes nothing")

	f.commit(t, "four")
	require.NoError(t, f.e.Flush(t.Context()))
	assert.Equal(t, 2, events.count("flush"), "one flush for the next save's asset")
	assert.Equal(t, []bool{false, true}, events.flushed)
}

// Test_Assets_DarwinFlushFailureFailsTheSave checks that an error of the final
// flush is a failed sync as an fsync's is: the save renames no snapshot, the
// error reaches the caller, and the service turns off, with no later save
// syncing or flushing again.
func Test_Assets_DarwinFlushFailureFailsTheSave(t *testing.T) {
	f := newSyncFixture(t, time.Hour)
	events := record(f.a, f.stateFile, fs.ErrPermission)

	f.commit(t, "body")

	require.ErrorIs(t, f.e.Flush(t.Context()), fs.ErrPermission)
	assert.NoFileExists(t, f.stateFile, "a save whose flush failed renames nothing")
	assert.Equal(t, 1, events.count("flush"))

	f.e.MarkDirty(syncService)
	require.ErrorIs(t, f.e.Flush(t.Context()), ErrUnsafe, "a flush that would now succeed does not make the asset durable")
	assert.Equal(t, 1, events.count("flush"), "nothing is flushed again")
	assert.False(t, f.e.Enabled(syncService), "health reports the service not persisted")
	assert.Len(t, f.logs.errors(), 1)
}
