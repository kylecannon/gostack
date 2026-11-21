package persist

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_Engine_MarkAbandonedKeepsItsRootOpenForTheFsyncs is review SQ-440's probe
// P11: a round whose fsyncs outlast its budget returns at the budget, and the
// fsyncs it left go on. The round closed the root they open the directories
// through, so each ended with "file already closed" and a warning that a marker
// could not be marked, though it was written. Here no fsync returns until the
// round has: the round returns at its budget, and when the fsyncs are let go no
// warning is logged and every marker is there.
func Test_Engine_MarkAbandonedKeepsItsRootOpenForTheFsyncs(t *testing.T) {
	e, dir, logs := reviewEngine(t, time.Hour)
	e.markBudget = 200 * time.Millisecond // time for the markers, not for the fsyncs

	var syncs atomic.Int32

	release := make(chan struct{})
	e.markSync = func(f *os.File) error {
		syncs.Add(1)
		<-release

		return f.Sync()
	}

	flushed := make(chan struct{})
	e.markFlush = func(f *os.File) error {
		defer close(flushed)

		return f.Sync()
	}

	names := []string{"alpha", "beta", "gamma", "delta"}

	e.markAbandoned(t.Context(), names) // returns at its budget, with every fsync still waiting
	close(release)

	select {
	case <-flushed:
	case <-time.After(10 * time.Second):
		t.Fatal("the fsyncs did not end")
	}

	for _, name := range names {
		assert.True(t, markedIn(dir, name), name)
	}

	// Each marker, its directory and the base dir, the directory being new.
	assert.Equal(t, int32(3*len(names)), syncs.Load())
	assert.Empty(t, logs.all(), "no marker was reported unmarked")
}

// Test_Engine_MarkAbandonedFlushesTheDriveOnceAfterTheSyncs checks the order of
// a round's durability: every marker and directory is synced before the single
// flush, and the flush is one call on the base dir.
func Test_Engine_MarkAbandonedFlushesTheDriveOnceAfterTheSyncs(t *testing.T) {
	e, dir, logs := reviewEngine(t, time.Hour)
	e.markBudget = time.Minute // the round waits for its fsyncs, however busy the disk

	var syncs, flushes, syncsAtFlush atomic.Int32

	e.markSync = func(f *os.File) error {
		syncs.Add(1)

		return f.Sync()
	}
	e.markFlush = func(f *os.File) error {
		flushes.Add(1)
		syncsAtFlush.Store(syncs.Load())

		info, err := f.Stat()
		require.NoError(t, err)

		base, err := os.Stat(dir)
		require.NoError(t, err)
		assert.True(t, os.SameFile(base, info), "the flush is on the base dir")

		return nil
	}

	e.markAbandoned(t.Context(), []string{"alpha", "beta", "gamma"})

	assert.Equal(t, int32(1), flushes.Load(), "one flush for the round")
	assert.Equal(t, int32(9), syncsAtFlush.Load(), "after every sync")
	assert.Empty(t, logs.all())
}

// Test_flushMarkers checks the flush of a round: ENOTSUP, which a filesystem
// with no drive flush answers, falls back on a sync of the base dir, and any
// other error is a failed flush.
func Test_flushMarkers(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)

	t.Cleanup(func() { _ = root.Close() })

	synced := 0
	fsync := func(*os.File) error { synced++; return nil }

	require.NoError(t, flushMarkers(root, fsync, func(*os.File) error { return nil }))
	assert.Zero(t, synced)

	require.NoError(t, flushMarkers(root, fsync, func(*os.File) error { return syscall.ENOTSUP }))
	assert.Equal(t, 1, synced, "the base dir is synced when the drive has no flush")

	err = flushMarkers(root, fsync, func(*os.File) error { return syscall.EIO })
	require.ErrorIs(t, err, syscall.EIO)
	assert.Contains(t, err.Error(), "flush the drive")

	err = flushMarkers(root, fsync, func(*os.File) error { return syscall.ENOTSUP })
	require.NoError(t, err)

	failing := errors.New("fsync failed")
	err = flushMarkers(root, func(*os.File) error { return failing }, func(*os.File) error { return syscall.ENOTSUP })
	require.ErrorIs(t, err, failing, "the fallback's own error is the flush's")

	require.NoError(t, root.Close())

	err = flushMarkers(root, fsync, func(*os.File) error { return nil })
	require.ErrorContains(t, err, "open the base dir to flush the drive")
}

// Test_marker_sync covers what a marker's sync fails with: its fsync, its close,
// the fsync of its directory and of the base dir.
func Test_marker_sync(t *testing.T) {
	made := func(t *testing.T, madeDir bool) (*os.Root, *marker, string) {
		t.Helper()

		dir := t.TempDir()
		root, err := os.OpenRoot(dir)
		require.NoError(t, err)
		t.Cleanup(func() { _ = root.Close() })

		m, err := writeAbandoned(t.Context(), root, "svc")
		require.NoError(t, err)
		require.NotNil(t, m)
		m.madeDir = madeDir

		return root, m, dir
	}

	t.Run("synced", func(t *testing.T) {
		root, m, _ := made(t, true)
		require.NoError(t, m.sync(root, syncFile))
	})

	t.Run("fsync fails", func(t *testing.T) {
		root, m, _ := made(t, false)
		err := m.sync(root, func(*os.File) error { return syscall.EIO })
		require.ErrorIs(t, err, syscall.EIO)
		assert.Contains(t, err.Error(), "sync the marker")
	})

	t.Run("close fails", func(t *testing.T) {
		root, m, _ := made(t, false)
		err := m.sync(root, func(f *os.File) error { return f.Close() }) // closed twice
		require.ErrorIs(t, err, os.ErrClosed)
		assert.Contains(t, err.Error(), "close the marker")
	})

	t.Run("directory gone", func(t *testing.T) {
		root, m, dir := made(t, false)
		require.NoError(t, os.RemoveAll(filepath.Join(dir, "svc")))

		err := m.sync(root, func(*os.File) error { return nil })
		require.ErrorIs(t, err, os.ErrNotExist)
		assert.Contains(t, err.Error(), "open svc")
	})

	t.Run("base dir fails", func(t *testing.T) {
		root, m, _ := made(t, true)

		calls := 0
		err := m.sync(root, func(*os.File) error {
			calls++
			if calls == 3 { // the marker, its directory, then the base dir
				return syscall.EIO
			}

			return nil
		})
		require.ErrorIs(t, err, syscall.EIO)
		assert.Contains(t, err.Error(), "sync .")
	})
}

// Test_Engine_MarkAbandonedRoundsShareOneBudget checks the allowance of a
// flush: three rounds under one ctx, each with fsyncs that take longer than the
// budget, together take the budget, not three times it, and the round that
// finds none left writes no marker and says so (SQ-440 N4). Without the
// allowance each round has the whole budget.
func Test_Engine_MarkAbandonedRoundsShareOneBudget(t *testing.T) {
	e, dir, logs := reviewEngine(t, time.Hour)
	e.markBudget = 60 * time.Millisecond

	var flushes atomic.Int32

	e.markSync = func(f *os.File) error {
		time.Sleep(80 * time.Millisecond)

		return f.Sync()
	}
	e.markFlush = func(*os.File) error { flushes.Add(1); return nil }

	ctx := withMarkAllowance(t.Context())
	start := time.Now()

	e.markAbandoned(ctx, []string{"a"})
	e.markAbandoned(ctx, []string{"b"})
	e.markAbandoned(ctx, []string{"c"})

	took := time.Since(start)
	t.Logf("three rounds under one allowance of %s took %s", e.markBudget, took)
	assert.Less(t, took, 90*time.Millisecond, "three rounds took no more than one budget, 60 ms, and the margin")
	assert.GreaterOrEqual(t, took, 55*time.Millisecond, "the first spent the budget on its fsyncs")

	assert.True(t, markedIn(dir, "a"))
	assert.False(t, markedIn(dir, "b"), "no time was left for the second round")
	assert.False(t, markedIn(dir, "c"))

	require.Eventually(t, func() bool { return len(logs.all()) >= 2 }, 10*time.Second, time.Millisecond,
		"the rounds that had no time say so")

	// A flush that marked at its deadline has what is left for the rounds after.
	other := withMarkAllowance(t.Context())
	e.markAbandoned(other, []string{"d"})
	assert.True(t, markedIn(dir, "d"), "another flush has its own allowance")

	// The fsyncs the rounds left go on; let them end before the dir is removed.
	require.Eventually(t, func() bool { return flushes.Load() == 2 }, 10*time.Second, time.Millisecond)
}
