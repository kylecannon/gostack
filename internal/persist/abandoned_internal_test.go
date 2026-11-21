package persist

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// abandonWarning is the start of the warning the next start logs.
const abandonWarning = "persist: a save was abandoned at its deadline before this start"

// abandonedMarker is where the fixture's service keeps its marker.
func (f *syncFixture) abandonedMarker() string {
	return filepath.Join(filepath.Dir(f.stateFile), abandonedFile)
}

// restart returns the log lines at warn level of a new engine on the fixture's
// base dir, once it has loaded: what the next start of the process says.
func (f *syncFixture) restart(t *testing.T) []string {
	t.Helper()

	logs := &warnings{}
	e := New(warnConfig(f.e.BaseDir(), time.Hour, logs))
	e.Register(syncService, &namer{})
	require.NoError(t, e.Load(t.Context()))

	return logs.all()
}

// abandon makes the save of the next Flush run out of time in an asset sync
// that does not return until release is called, and returns release.
func (f *syncFixture) abandon(t *testing.T) (release func()) {
	t.Helper()

	_, release = gateFirstSync(f.a, nil)

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	require.ErrorIs(t, f.e.Flush(ctx), context.DeadlineExceeded)

	return release
}

// Test_Engine_AbandonedSaveIsMarkedForTheNextStart checks that a save that runs
// out of time leaves a durable marker, that the next start logs one warning
// naming the service and the time of its last completed save, and that the next
// save that completes removes the marker, so the start after it is quiet.
func Test_Engine_AbandonedSaveIsMarkedForTheNextStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSyncFixture(t, time.Hour)
		f.commit(t, "saved")
		require.NoError(t, f.e.Flush(t.Context()))
		assert.NoFileExists(t, f.abandonedMarker(), "a completed save leaves no marker")
		assert.Empty(t, f.restart(t), "and the start after it says nothing")

		info, err := os.Stat(f.stateFile)
		require.NoError(t, err)

		f.commit(t, "late")
		release := f.abandon(t)

		defer release()

		assert.FileExists(t, f.abandonedMarker(), "the abandoned save is marked before the flush returns")

		warns := f.restart(t)
		require.Len(t, warns, 1, "one warning for the start")
		assert.True(t, strings.HasPrefix(warns[0], abandonWarning), warns[0])
		assert.Contains(t, warns[0], "services="+"["+syncService+"]")
		assert.Contains(t, warns[0], "last_completed_save=["+syncService+"="+info.ModTime().UTC().Format(time.RFC3339)+"]")

		release()
		synctest.Wait()

		require.NoError(t, f.e.Flush(t.Context()))
		assert.NoFileExists(t, f.abandonedMarker(), "the save that completes removes the marker")
		assert.Empty(t, f.restart(t), "so the next start says nothing")
	})
}

// Test_Engine_AbandonedFirstSaveIsMarked checks the marker of a service that
// never completed a save: its directory is made for the marker, and the warning
// says so.
func Test_Engine_AbandonedFirstSaveIsMarked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSyncFixture(t, time.Hour)
		f.commit(t, "late")

		release := f.abandon(t)

		defer release()

		assert.NoFileExists(t, f.stateFile)
		assert.FileExists(t, f.abandonedMarker())

		warns := f.restart(t)
		require.Len(t, warns, 1)
		assert.Contains(t, warns[0], "last_completed_save=["+syncService+"=never]")
	})
}

// Test_Engine_CleanShutdownLeavesNoMarker checks that a shutdown whose save
// completes leaves no marker and the next start says nothing.
func Test_Engine_CleanShutdownLeavesNoMarker(t *testing.T) {
	f := newSyncFixture(t, time.Hour)
	f.commit(t, "body")

	require.NoError(t, f.e.Close(t.Context()))
	assert.NoFileExists(t, f.abandonedMarker())
	assert.Empty(t, f.restart(t))
}
