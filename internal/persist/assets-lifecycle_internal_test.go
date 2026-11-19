package persist

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_Assets_OpenErrorInSyncIsNotFinal checks that a per-file sync that cannot
// open an asset (EACCES here; EMFILE, ENFILE or EINTR in the field) fails only
// that save: no write was lost, so the ids stay pending and the next save syncs
// them, with the service still saved (SQ-190 item 2).
func Test_Assets_OpenErrorInSyncIsNotFinal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file modes do not bind root")
	}

	dir := t.TempDir()
	e := New(quietConfig(dir, time.Hour))
	n := &namer{}
	e.Register(syncService, n)

	a, err := e.Assets(syncService)
	require.NoError(t, err)

	forcePerFile(t, a)

	id := commitAsset(t, a, "body")
	n.add(id)
	e.MarkDirty(syncService)

	fan := filepath.Join(a.dir, blobsDir, id[:2])
	require.NoError(t, os.Chmod(fan, 0))
	t.Cleanup(func() { _ = os.Chmod(fan, 0o750) })

	err = e.Flush(t.Context())
	require.ErrorIs(t, err, fs.ErrPermission)
	require.NotErrorIs(t, err, errSyncFailed)
	assert.True(t, e.Enabled(syncService), "an open error is not a lost write")
	assert.NoFileExists(t, filepath.Join(dir, syncService, "store.json"), "the save that could not sync renames nothing")

	require.NoError(t, os.Chmod(fan, 0o750)) // the condition clears

	synced := recordSyncs(a, filepath.Join(dir, syncService, "store.json"))

	e.mu.Lock()
	e.retryAt = time.Time{}
	e.mu.Unlock()
	require.NoError(t, e.Flush(t.Context()))

	assert.True(t, e.Enabled(syncService))
	assert.Equal(t, id, readState(t, filepath.Join(dir, syncService, "store.json")))
	assert.NotEmpty(t, synced(), "the next save syncs the ids the failed one left pending")
}

// remover is a namer whose Restore runs a hook, as S3's Restore Removes the
// bodies of the buckets it replaces.
type remover struct {
	namer

	onRestore func()
}

func (r *remover) Restore(io.Reader) error {
	if r.onRestore != nil {
		r.onRestore()
	}

	return nil
}

// Test_Engine_Load_SchedulesTheSaveThatDrainsRemovals checks that the bodies a
// runtime Load queues for removal are deleted by a save the Load schedules,
// not left until the next write (SQ-190 item 7).
func Test_Engine_Load_SchedulesTheSaveThatDrainsRemovals(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := New(quietConfig(t.TempDir(), 10*time.Millisecond))
		r := &remover{}
		e.Register(syncService, r)

		a, err := e.Assets(syncService)
		require.NoError(t, err)

		replaced := commitAsset(t, a, "replaced")
		r.add(replaced)
		e.MarkDirty(syncService)
		require.NoError(t, e.Flush(t.Context()))

		r.onRestore = func() {
			r.mu.Lock()
			r.ids = nil
			r.mu.Unlock()
			assert.NoError(t, a.Remove(replaced))
		}

		require.NoError(t, e.Load(t.Context()))
		assert.FileExists(t, filepath.Join(a.dir, mustPath(t, replaced)), "the file on disk names it until a save")

		time.Sleep(time.Second)
		synctest.Wait()

		assert.NoFileExists(t, filepath.Join(a.dir, mustPath(t, replaced)), "a save after the load drained the removal")
	})
}

func mustPath(t *testing.T, id string) string {
	t.Helper()

	path, err := assetPath(id)
	require.NoError(t, err)

	return path
}

// failSync makes every sync of a fail as a lost write does.
func failSync(a *Assets) {
	a.fsync = func(*os.File) error { return errors.New("input/output error") }
}

// Test_Assets_FailedStateIsBounded checks that once a sync has failed, the
// assets the service keeps committing and removing leave no trace in the
// tracking sets: nothing will sync or sweep them before a restart (SQ-190 item
// 8). A body the last saved snapshot names is kept: a restart loads that
// snapshot.
func Test_Assets_FailedStateIsBounded(t *testing.T) {
	dir := t.TempDir()
	e := New(quietConfig(dir, time.Hour))
	n := &namer{}
	e.Register(syncService, n)

	a, err := e.Assets(syncService)
	require.NoError(t, err)

	saved := commitAsset(t, a, "saved")
	n.add(saved)
	e.MarkDirty(syncService)
	require.NoError(t, e.Flush(t.Context()))

	failSync(a)
	n.add(commitAsset(t, a, "lost"))
	e.MarkDirty(syncService)
	require.ErrorIs(t, e.Flush(t.Context()), errSyncFailed)
	require.False(t, e.Enabled(syncService))

	later := make([]string, 0, 100)
	for range 100 {
		later = append(later, commitAsset(t, a, "after"))
	}

	a.mu.Lock()
	assert.Empty(t, a.unsynced, "no sync will run")
	assert.Empty(t, a.committed, "no sweep will run")
	a.mu.Unlock()

	for _, id := range later {
		require.NoError(t, a.Remove(id))
		assert.NoFileExists(t, filepath.Join(a.dir, mustPath(t, id)), "no snapshot on disk names it")
	}

	require.NoError(t, a.Remove(saved))
	assert.FileExists(t, filepath.Join(a.dir, mustPath(t, saved)), "the state file on disk names it")

	a.mu.Lock()
	defer a.mu.Unlock()

	assert.Empty(t, a.fresh)
	assert.Equal(t, map[string]struct{}{saved: {}}, a.removed,
		"only removals of bodies a saved snapshot may name wait, for the restart")
}

// openFiles counts this process's open descriptors.
func openFiles(t *testing.T) int {
	t.Helper()

	entries, err := os.ReadDir("/dev/fd")
	require.NoError(t, err)

	return len(entries)
}

// Test_Engine_Close_ReleasesAssets checks that Close releases what the assets
// hold open: the root on the assets directory and, where syncfs is used, the
// directory kept open for it (SQ-190 item 9).
func Test_Engine_Close_ReleasesAssets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /dev/fd")
	}

	before := openFiles(t)

	e := New(quietConfig(t.TempDir(), time.Hour))
	n := &namer{}
	e.Register(syncService, n)

	a, err := e.Assets(syncService)
	require.NoError(t, err)

	n.add(commitAsset(t, a, "body"))
	e.MarkDirty(syncService)
	require.NoError(t, e.Stop(t.Context()))
	assert.Greater(t, openFiles(t), before, "the assets stay open after Stop, for the last flush")

	n.add(commitAsset(t, a, "late"))
	e.MarkDirty(syncService)
	require.NoError(t, e.Close(t.Context()))

	assert.Equal(t, before, openFiles(t), "Close leaves no descriptor open")
}
