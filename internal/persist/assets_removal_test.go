package persist_test

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kylecannon/gostack/internal/persist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_Assets_LiveLoad_KeepsWhatThisProcessCommitted: a body is written before a
// save and committed after it, so its file is older than the state file. A file
// time says nothing about the commit, so the sweep keeps it by the id.
func Test_Assets_LiveLoad_KeepsWhatThisProcessCommitted(t *testing.T) {
	t.Run("a body written before a save and committed after it", func(t *testing.T) {
		dir := t.TempDir()
		e, s, a := openShelf(t, dir)

		w, err := a.Create()
		require.NoError(t, err)

		_, err = io.WriteString(w, "slow upload body")
		require.NoError(t, err)

		past := time.Now().Add(-time.Hour)
		tmp := filepath.Join(assetsOf(dir), "tmp")
		require.NoError(t, os.Chtimes(filepath.Join(tmp, tempFiles(t, dir)[0]), past, past))

		s.add(put(t, a, "other"))
		flush(t, e)

		id, _, err := w.Commit()
		require.NoError(t, err)

		load(t, e)
		s.add(id)

		assert.FileExists(t, blobFile(dir, id), "committed after the snapshot, and not swept for its old file time")
	})

	t.Run("a state file with a future time removes nothing this process committed", func(t *testing.T) {
		dir := t.TempDir()
		e, s, a := openShelf(t, dir)

		flush(t, e)

		id := put(t, a, "not named yet")
		past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
		require.NoError(t, os.Chtimes(blobFile(dir, id), past, past))
		require.NoError(t, os.Chtimes(filepath.Join(dir, shelfService, jsonFile), future, future))

		load(t, e)
		s.add(id)

		assert.FileExists(t, blobFile(dir, id))
	})

	t.Run("a clock that went backwards", func(t *testing.T) {
		dir := t.TempDir()
		e, _, a := openShelf(t, dir)

		flush(t, e)

		id := put(t, a, "committed after the snapshot")
		past := time.Now().Add(-48 * time.Hour)
		require.NoError(t, os.Chtimes(blobFile(dir, id), past, past))

		load(t, e)

		assert.FileExists(t, blobFile(dir, id))
	})
}

// Test_Assets_CommittedSetIsBounded: an id leaves the set that shields it from a
// live load once a saved state names it, and once it is removed.
func Test_Assets_CommittedSetIsBounded(t *testing.T) {
	t.Run("a saved state names it", func(t *testing.T) {
		dir := t.TempDir()
		e, s, a := openShelf(t, dir)

		id := put(t, a, "body")
		s.add(id)
		flush(t, e)

		// The state loses the id without a Remove, which leaves the file behind.
		s.drop(id)
		flush(t, e)
		load(t, e)

		assert.NoFileExists(t, blobFile(dir, id), "named by a durable snapshot once, so no longer shielded")
	})

	t.Run("it is removed", func(t *testing.T) {
		dir := t.TempDir()
		e, s, a := openShelf(t, dir)

		id := put(t, a, "body")
		s.add(id)
		s.drop(id)
		require.NoError(t, a.Remove(id))
		assert.NoFileExists(t, blobFile(dir, id))

		// A copy that comes back as an orphan is no longer shielded.
		require.NoError(t, os.WriteFile(blobFile(dir, id), []byte("back"), 0o600))
		load(t, e)

		assert.NoFileExists(t, blobFile(dir, id))
	})
}

// Test_Assets_RemoveOfAnAssetNoSnapshotNames: an asset committed after the last
// save ended is named by no snapshot, so Remove frees it at once, and a failing
// save blocks only the removals of what a saved snapshot names.
func Test_Assets_RemoveOfAnAssetNoSnapshotNames(t *testing.T) {
	dir := t.TempDir()
	e, s, a := openShelf(t, dir)

	saved := put(t, a, "named by the saved snapshot")
	s.add(saved)
	flush(t, e)

	fresh := put(t, a, "committed after the last save")
	s.add(fresh)

	// The disk is full: every save fails.
	s.set(nil, errSnapshot)

	s.drop(fresh)
	require.NoError(t, a.Remove(fresh))
	assert.NoFileExists(t, blobFile(dir, fresh), "freed at once: no snapshot names it")

	s.drop(saved)
	require.NoError(t, a.Remove(saved))
	assert.FileExists(t, blobFile(dir, saved), "the snapshot on disk names it, so it waits")

	e.MarkDirty(shelfService)
	require.ErrorIs(t, e.Flush(t.Context()), errSnapshot)
	assert.FileExists(t, blobFile(dir, saved), "a failed save deletes nothing")

	s.set(nil, nil)
	require.NoError(t, e.Flush(t.Context()))
	assert.NoFileExists(t, blobFile(dir, saved), "the next save that works frees it")
}

// Test_Assets_RemoveOfAnAssetASaveMayName: an asset committed while a snapshot
// is written, or before a save that then failed, may be named by a snapshot
// that reaches the disk, so its removal waits for the next save.
func Test_Assets_RemoveOfAnAssetASaveMayName(t *testing.T) {
	t.Run("committed while the snapshot is written", func(t *testing.T) {
		dir := t.TempDir()
		e, s, a := openShelf(t, dir)

		var id string

		// The write commits and names its asset after the save began and before the
		// state is read, so the snapshot names it.
		s.setBefore(func() {
			id = put(t, a, "committed during the save")
			s.add(id)
		})
		flush(t, e)

		assert.Contains(t, readFile(t, filepath.Join(dir, shelfService, jsonFile)), id)

		s.drop(id)
		require.NoError(t, a.Remove(id))
		assert.FileExists(t, blobFile(dir, id), "the snapshot on disk may name it")

		flush(t, e)
		assert.NoFileExists(t, blobFile(dir, id))
	})

	t.Run("committed before a save that failed", func(t *testing.T) {
		dir := t.TempDir()
		e, s, a := openShelf(t, dir)

		id := put(t, a, "body")
		s.add(id)

		s.set(nil, errSnapshot)
		e.MarkDirty(shelfService)
		require.ErrorIs(t, e.Flush(t.Context()), errSnapshot)

		s.drop(id)
		require.NoError(t, a.Remove(id))
		assert.FileExists(t, blobFile(dir, id), "a failed save may have left a snapshot that names it")

		s.set(nil, nil)
		flush(t, e)
		assert.NoFileExists(t, blobFile(dir, id))
	})
}

// Test_Assets_StuckRemovalsAreLogged: a removal queue that cannot drain is
// logged once for each change, with its length and the error.
func Test_Assets_StuckRemovalsAreLogged(t *testing.T) {
	var logs syncBuffer

	dir := t.TempDir()
	e := persist.New(loggedCfg(dir, &logs))
	s := newShelf()
	e.Register(shelfService, s)

	a, err := e.Assets(shelfService)
	require.NoError(t, err)

	ids := make([]string, 0, 2)

	for i := range 2 {
		id := put(t, a, fmt.Sprintf("body %d", i))
		s.add(id)

		ids = append(ids, id)
	}

	flush(t, e)

	for _, id := range ids {
		s.drop(id)
		require.NoError(t, a.Remove(id))
	}

	s.set(nil, errSnapshot)

	for range 3 {
		e.MarkDirty(shelfService)
		require.ErrorIs(t, e.Flush(t.Context()), errSnapshot)
	}

	const stuck = "asset removals are waiting for a save that cannot finish"

	assert.Equal(t, 1, strings.Count(logs.String(), stuck), "once, not once per failed save")
	assert.Contains(t, logs.String(), "queued=2")
	assert.Contains(t, logs.String(), "error=")

	s.set(nil, nil)
	require.NoError(t, e.Flush(t.Context()))

	assert.Contains(t, logs.String(), "asset removals are going through again")
}

// Test_Assets_LinkInsideTheAssetsDir: tmp, blobs and each fanout directory must
// be real directories. A link from a fanout directory to tmp would send a
// commit into the directory Load empties.
func Test_Assets_LinkInsideTheAssetsDir(t *testing.T) {
	tests := []struct {
		name string
		link func(t *testing.T, assets string)
	}{
		{"a fanout directory linked to tmp", func(t *testing.T, assets string) {
			t.Helper()
			require.NoError(t, os.MkdirAll(filepath.Join(assets, "tmp"), 0o750))
			require.NoError(t, os.MkdirAll(filepath.Join(assets, "blobs"), 0o750))

			for i := range 256 {
				require.NoError(t, os.Symlink("../tmp", filepath.Join(assets, "blobs", fmt.Sprintf("%02x", i))))
			}
		}},
		{"one fanout directory linked to tmp", func(t *testing.T, assets string) {
			t.Helper()
			require.NoError(t, os.MkdirAll(filepath.Join(assets, "tmp"), 0o750))
			require.NoError(t, os.MkdirAll(filepath.Join(assets, "blobs"), 0o750))
			require.NoError(t, os.Symlink("../tmp", filepath.Join(assets, "blobs", "7f")))
		}},
		{"tmp linked to blobs", func(t *testing.T, assets string) {
			t.Helper()
			require.NoError(t, os.MkdirAll(filepath.Join(assets, "blobs"), 0o750))
			require.NoError(t, os.Symlink("blobs", filepath.Join(assets, "tmp")))
		}},
		{"blobs linked to a directory inside", func(t *testing.T, assets string) {
			t.Helper()
			require.NoError(t, os.MkdirAll(filepath.Join(assets, "elsewhere"), 0o750))
			require.NoError(t, os.Symlink("elsewhere", filepath.Join(assets, "blobs")))
		}},
		{"a fanout directory that is a file", func(t *testing.T, assets string) {
			t.Helper()
			require.NoError(t, os.MkdirAll(filepath.Join(assets, "blobs"), 0o750))
			require.NoError(t, os.WriteFile(filepath.Join(assets, "blobs", "00"), nil, 0o600))
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.MkdirAll(assetsOf(dir), 0o750))
			tt.link(t, assetsOf(dir))

			e := persist.New(baseCfg(dir))
			s := newShelf()
			e.Register(shelfService, s)

			a, err := e.Assets(shelfService)
			require.ErrorIs(t, err, persist.ErrUnsafe)
			assert.Nil(t, a)
			assert.False(t, e.Enabled(shelfService), "health reports the service as not persisted")

			require.ErrorIs(t, e.Load(t.Context()), persist.ErrUnsafe)
			assert.Empty(t, tempFiles(t, dir), "nothing was committed into tmp")
		})
	}
}
